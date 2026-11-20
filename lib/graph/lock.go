package graph

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"indicer/lib/cnst"
)

const (
	lockFileName = "dues.lock"

	// Each holder records its identity in a sibling file of its own,
	// dues.lock.<pid>-<random>.info, rather than inside the locked file.
	// Reading a byte range that another process holds locked is portable only
	// if the lock and the read agree about sharing modes, and this metadata is
	// diagnostic: it exists to turn "resource busy" into a sentence naming a
	// process. A file per holder is what lets a shared holder remove its own
	// record without removing the other holders' (GRA-8).
	lockInfoPrefix = "dues.lock."
	lockInfoSuffix = ".info"

	// legacyLockInfoFileName is the single record earlier releases wrote.
	legacyLockInfoFileName = "dues.lock.info"

	// grapheneLockFileName is the lock graphene takes in the directory too.
	grapheneLockFileName = "graphene.lock"
)

// IsLockFile reports whether name, a file in a graph directory, is a lock
// or a lock holder's record rather than graph data. A copy of the store
// leaves these out: they describe the processes using this copy.
func IsLockFile(name string) bool {
	return name == lockFileName || name == grapheneLockFileName || strings.HasPrefix(name, lockInfoPrefix)
}

// ErrGraphLocked is returned when another process holds the store.
type ErrGraphLocked struct {
	Dir    string
	Holder *lockInfo // nil when no holder recorded itself

	// err is graphene's own lock error, when graphene refused the open.
	err error
}

func (e *ErrGraphLocked) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "graph store at %s is in use", e.Dir)
	if e.Holder != nil {
		fmt.Fprintf(&b, " by %s (pid %d", e.Holder.Command, e.Holder.PID)
		if !e.Holder.Started.IsZero() {
			fmt.Fprintf(&b, ", started %s", e.Holder.Started.Local().Format("15:04:05"))
		}
		b.WriteString(")")
	} else {
		b.WriteString(" by another process")
	}
	b.WriteString("; wait for it to finish, or close it, and try again")
	return b.String()
}

func (e *ErrGraphLocked) Unwrap() error { return e.err }

type lockInfo struct {
	PID     int       `json:"pid"`
	Command string    `json:"command"`
	Started time.Time `json:"started"`
	Shared  bool      `json:"shared"`
}

// fileLock is an advisory whole-file lock over <graphdir>/dues.lock.
//
// graphene takes a lock of its own since v0.9.0, and this one is kept beside
// it for one release so that a DUES binary from before then, which knows only
// dues.lock, is still excluded. It is advisory: it stops a second dues, not a
// text editor.
type fileLock struct {
	dir      string
	path     string
	infoPath string
	file     *os.File
	shared   bool
	held     bool
}

// acquireLock takes the lock for dir. An exclusive lock, for anything that
// writes, creates the directory if needed; a shared lock, which concurrent
// readers take, needs the directory to exist.
//
// On media nothing can be written to, a shared lock opens an existing lock
// file read-only, and when there is no lock file to open it takes none: no
// writer can exist there to be excluded (CFG-12).
//
// It never blocks: a store held by another process fails immediately with
// *ErrGraphLocked rather than leaving an examiner staring at a hung command.
func acquireLock(dir string, shared bool) (*fileLock, error) {
	if !shared {
		if err := os.MkdirAll(dir, cnst.DirPerm); err != nil {
			return nil, fmt.Errorf("graph lock: create %s: %w", dir, err)
		}
	}

	l := &fileLock{
		dir:    dir,
		path:   filepath.Join(dir, lockFileName),
		shared: shared,
	}

	file, err := os.OpenFile(l.path, os.O_CREATE|os.O_RDWR, 0o644)
	writable := err == nil
	if err != nil && shared && isReadOnlyMedia(err) {
		file, err = os.Open(l.path)
		if errors.Is(err, fs.ErrNotExist) {
			return l, nil
		}
	}
	if err != nil {
		return nil, fmt.Errorf("graph lock: open %s: %w", l.path, err)
	}
	l.file = file

	if err := lockFile(file, shared); err != nil {
		file.Close()
		if isLockContended(err) {
			return nil, &ErrGraphLocked{Dir: dir, Holder: readLockInfo(dir)}
		}
		return nil, fmt.Errorf("graph lock: %s: %w", l.path, err)
	}
	l.held = true

	if writable {
		if !shared {
			// Holding the lock exclusively means nobody else holds it, so any
			// other record is left by a process that died holding it.
			removeLockInfo(dir)
		}
		l.writeInfo()
	}
	return l, nil
}

func (l *fileLock) release() error {
	if l == nil || !l.held {
		return nil
	}
	l.held = false

	// Remove the record before dropping the lock, so a process that acquires
	// immediately afterwards cannot read our stale identity and report it.
	if l.infoPath != "" {
		_ = os.Remove(l.infoPath)
	}

	unlockErr := unlockFile(l.file)
	closeErr := l.file.Close()
	if unlockErr != nil {
		return fmt.Errorf("graph lock: release %s: %w", l.path, unlockErr)
	}
	if closeErr != nil {
		return fmt.Errorf("graph lock: close %s: %w", l.path, closeErr)
	}
	return nil
}

// writeInfo records who holds the lock. Failure is ignored: the record is a
// courtesy for the error message on the *next* acquisition, and refusing to
// open an otherwise healthy store because a diagnostic file could not be
// written would be a worse outcome than a vaguer error later.
func (l *fileLock) writeInfo() {
	command := "dues"
	if len(os.Args) > 0 {
		command = filepath.Base(os.Args[0])
		if len(os.Args) > 1 {
			command += " " + os.Args[1]
		}
	}

	encoded, err := json.Marshal(lockInfo{
		PID:     os.Getpid(),
		Command: command,
		Started: time.Now().UTC(),
		Shared:  l.shared,
	})
	if err != nil {
		return
	}
	var suffix [4]byte
	_, _ = rand.Read(suffix[:])
	path := filepath.Join(l.dir, fmt.Sprintf("%s%d-%s%s", lockInfoPrefix, os.Getpid(), hex.EncodeToString(suffix[:]), lockInfoSuffix))
	if err := os.WriteFile(path, encoded, 0o644); err == nil {
		l.infoPath = path
	}
}

// lockInfoFiles lists the holder records in dir, including the single record
// of earlier releases.
func lockInfoFiles(dir string) []string {
	matches, _ := filepath.Glob(filepath.Join(dir, lockInfoPrefix+"*"+lockInfoSuffix))
	if _, err := os.Stat(filepath.Join(dir, legacyLockInfoFileName)); err == nil {
		matches = append(matches, filepath.Join(dir, legacyLockInfoFileName))
	}
	return matches
}

func removeLockInfo(dir string) {
	for _, path := range lockInfoFiles(dir) {
		_ = os.Remove(path)
	}
}

// readLockInfo names a holder of dir's lock: an exclusive holder when one has
// recorded itself, else the most recent shared holder.
func readLockInfo(dir string) *lockInfo {
	var best *lockInfo
	for _, path := range lockInfoFiles(dir) {
		raw, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var info lockInfo
		if err := json.Unmarshal(raw, &info); err != nil {
			continue
		}
		switch {
		case best == nil,
			best.Shared && !info.Shared,
			best.Shared == info.Shared && info.Started.After(best.Started):
			best = &info
		}
	}
	return best
}

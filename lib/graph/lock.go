package graph

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"indicer/lib/cnst"
)

const (
	lockFileName = "dues.lock"
	// The holder's identity lives in a sibling file rather than inside the
	// locked one. Reading a byte range that another process holds locked is
	// portable only if the lock and the read agree about sharing modes, and
	// this metadata is diagnostic — it exists to turn "resource busy" into a
	// sentence naming a process. A separate file keeps the lock itself a plain
	// whole-file advisory lock on every platform.
	lockInfoFileName = "dues.lock.info"
)

// ErrGraphLocked is returned when another process holds the store.
type ErrGraphLocked struct {
	Dir    string
	Holder *lockInfo // nil when the sibling metadata file was unreadable
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
	b.WriteString("; a graphene store has no internal locking and two writers will corrupt it")
	return b.String()
}

type lockInfo struct {
	PID     int       `json:"pid"`
	Command string    `json:"command"`
	Started time.Time `json:"started"`
	Shared  bool      `json:"shared"`
}

// fileLock is an advisory whole-file lock over <graphdir>/dues.lock.
//
// GrapheneDB ships no locking of its own, so nothing in the library stops two
// processes opening one store directory and interleaving WAL appends. This
// supplies the missing exclusion. It is advisory: it stops a second dues, not a
// text editor.
type fileLock struct {
	dir      string
	path     string
	infoPath string
	file     *os.File
	shared   bool
	held     bool
}

// acquireLock takes the lock for dir, creating the directory if needed. A
// shared lock permits concurrent readers and is used for --read-only opens; an
// exclusive lock is required for anything that writes.
//
// It never blocks: a store held by another process fails immediately with
// *ErrGraphLocked rather than leaving an examiner staring at a hung command.
func acquireLock(dir string, shared bool) (*fileLock, error) {
	if err := os.MkdirAll(dir, cnst.DirPerm); err != nil {
		return nil, fmt.Errorf("graph lock: create %s: %w", dir, err)
	}

	l := &fileLock{
		dir:      dir,
		path:     filepath.Join(dir, lockFileName),
		infoPath: filepath.Join(dir, lockInfoFileName),
		shared:   shared,
	}

	file, err := os.OpenFile(l.path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("graph lock: open %s: %w", l.path, err)
	}
	l.file = file

	if err := lockFile(file, shared); err != nil {
		file.Close()
		if isLockContended(err) {
			return nil, &ErrGraphLocked{Dir: dir, Holder: readLockInfo(l.infoPath)}
		}
		return nil, fmt.Errorf("graph lock: %s: %w", l.path, err)
	}
	l.held = true

	l.writeInfo()
	return l, nil
}

func (l *fileLock) release() error {
	if l == nil || !l.held {
		return nil
	}
	l.held = false

	// Remove the metadata before dropping the lock, so a process that acquires
	// immediately afterwards cannot read our stale identity and report it.
	_ = os.Remove(l.infoPath)

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

// writeInfo records who holds the lock. Failure is ignored: the metadata is a
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
	_ = os.WriteFile(l.infoPath, encoded, 0o644)
}

func readLockInfo(path string) *lockInfo {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var info lockInfo
	if err := json.Unmarshal(raw, &info); err != nil {
		return nil
	}
	return &info
}

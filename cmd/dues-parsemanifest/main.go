// Command dues-parsemanifest walks a disk image with the DUES parser stack and
// writes one JSON object per line describing every partition and file it
// finds. It is a measurement tool, not part of the product: its manifests are
// the golden baselines the correctness pass diffs parser changes against.
//
// Each file line records what the parser reports (path, kind, size, flags,
// data runs) and, optionally, content digests computed two independent ways:
// from the data runs read off the image, and from the adapter's own Open.
// It also records what the ingest indexer would register for the file today,
// so a manifest shows both "what is on the volume" and "what DUES keeps".
//
// The walk never aborts on a recoverable problem. Open, walk, stat and read
// failures, including panics inside a parsing library, are written to the
// manifest as error fields and the walk continues.
//
// Usage:
//
//	dues-parsemanifest [-hash none|runs|open|both] [-max-hash N] [-info] [-o out.jsonl] IMAGE
//	dues-parsemanifest diff [-allow allow.json] BEFORE.jsonl AFTER.jsonl
//
// diff compares two manifests of the same image and counts differences by
// category (files added or removed, content changed, newly indexed, ...).
// With an allowlist mapping image base names to the categories an intended
// change may produce, any other difference makes it exit 1.
package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"runtime/debug"
	"strings"
	"time"

	"github.com/aoiflux/libvhdi"

	"indicer/lib/parser/core"
	"indicer/lib/parser/fs"
	"indicer/lib/parser/image"
	"indicer/lib/parser/partition"
)

// SchemaVersion identifies the manifest line format. Bump it when a field
// changes meaning, so a diff never compares two different shapes.
const SchemaVersion = 1

type imageLine struct {
	T          string `json:"t"`
	Schema     int    `json:"schema"`
	Path       string `json:"path"`
	Format     string `json:"format,omitempty"`
	Size       int64  `json:"size,omitempty"`
	Scheme     string `json:"scheme,omitempty"`
	Partitions int    `json:"partitions"`
	Error      string `json:"error,omitempty"`

	VHD *vhdInfo `json:"vhd,omitempty"`

	Tool    string `json:"tool"`
	Started string `json:"started"`
}

type vhdInfo struct {
	Differencing  bool   `json:"differencing"`
	ChainDepth    int    `json:"chain_depth"`
	ChainComplete bool   `json:"chain_complete"`
	Parent        string `json:"parent,omitempty"`
	ResolveError  string `json:"resolve_error,omitempty"`
	Dirty         bool   `json:"dirty"`
	LogReplayed   bool   `json:"log_replayed"`
}

type partitionLine struct {
	T         string `json:"t"`
	Index     int    `json:"index"`
	Offset    int64  `json:"offset"`
	Size      int64  `json:"size"`
	TypeName  string `json:"type_name,omitempty"`
	Name      string `json:"name,omitempty"`
	Allocated bool   `json:"allocated"`
	FS        string `json:"fs,omitempty"`
	OpenError string `json:"open_error,omitempty"`
	WalkError string `json:"walk_error,omitempty"`
	Panic     string `json:"panic,omitempty"`
}

type fileLine struct {
	T          string `json:"t"`
	Part       int    `json:"part"`
	PartOffset int64  `json:"part_offset"`
	Path       string `json:"path"`
	Kind       string `json:"kind"`
	Size       int64  `json:"size"`
	Deleted    bool   `json:"deleted"`
	Fragmented bool   `json:"fragmented"`
	Inode      uint64 `json:"inode"`

	Runs        int   `json:"runs"`
	SparseRuns  int   `json:"sparse_runs,omitempty"`
	RunBytes    int64 `json:"run_bytes"`
	RunsInImage bool  `json:"runs_in_image"`
	// RunsShort marks a file whose runs cover fewer bytes than its size, so
	// no runs digest is recorded for it.
	RunsShort bool `json:"runs_short,omitempty"`
	Streams   int  `json:"streams,omitempty"`

	// The W5 content contract: how the content is reached, its provenance
	// flags, and how many bytes of it were located.
	ContentKind string `json:"content_kind,omitempty"`
	Prov        string `json:"prov,omitempty"`
	Located     int64  `json:"located,omitempty"`

	// DuesIndexed reports whether the ingest indexer as of this baseline would
	// register the file (non-fragmented, with at least one located run), and
	// DuesRangeLen the byte length it would register. The indexer reads the
	// raw run, not the run truncated to Size.
	DuesIndexed  bool  `json:"dues_indexed"`
	DuesRangeLen int64 `json:"dues_range_len,omitempty"`

	SHA256Runs  string `json:"sha256_runs,omitempty"`
	SHA256Open  string `json:"sha256_open,omitempty"`
	OpenBytes   int64  `json:"open_bytes,omitempty"`
	SHA256Dues  string `json:"sha256_dues,omitempty"`
	HashSkipped string `json:"hash_skipped,omitempty"`

	StatError  string `json:"stat_error,omitempty"`
	RunsError  string `json:"runs_error,omitempty"`
	ReadError  string `json:"read_error,omitempty"`
	OpenError  string `json:"open_error,omitempty"`
	OpenPanic  string `json:"open_panic,omitempty"`
	StatPanic  string `json:"stat_panic,omitempty"`
	RunsPanic  string `json:"runs_panic,omitempty"`
	ReadPanic  string `json:"read_panic,omitempty"`
	StreamsErr string `json:"streams_error,omitempty"`
}

type summaryLine struct {
	T          string  `json:"t"`
	Partitions int     `json:"partitions"`
	Files      int     `json:"files"`
	Dirs       int     `json:"dirs"`
	Other      int     `json:"other"`
	Deleted    int     `json:"deleted"`
	Fragmented int     `json:"fragmented"`
	Indexed    int     `json:"dues_indexed"`
	Errors     int     `json:"errors"`
	Panics     int     `json:"panics"`
	Seconds    float64 `json:"seconds"`
}

type options struct {
	hash     string
	maxHash  int64
	infoOnly bool
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 && args[0] == "diff" {
		return runDiff(args[1:], stdout, stderr)
	}
	flags := flag.NewFlagSet("dues-parsemanifest", flag.ContinueOnError)
	flags.SetOutput(stderr)
	hash := flags.String("hash", "both", "content digests to compute: none, runs, open or both")
	maxHash := flags.Int64("max-hash", 0, "skip digests for files larger than this many bytes (0 = no limit)")
	out := flags.String("o", "", "write the manifest here instead of stdout")
	infoOnly := flags.Bool("info", false, "write only the image line (format, size, VHD chain) and stop")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 1 {
		fmt.Fprintln(stderr, "usage: dues-parsemanifest [-hash none|runs|open|both] [-max-hash N] [-o out.jsonl] IMAGE")
		return 2
	}
	switch *hash {
	case "none", "runs", "open", "both":
	default:
		fmt.Fprintf(stderr, "dues-parsemanifest: -hash must be none, runs, open or both, not %q\n", *hash)
		return 2
	}
	if *maxHash < 0 {
		fmt.Fprintln(stderr, "dues-parsemanifest: -max-hash must not be negative")
		return 2
	}

	w := stdout
	var outFile *os.File
	if *out != "" {
		f, err := os.Create(*out)
		if err != nil {
			fmt.Fprintf(stderr, "dues-parsemanifest: cannot create output: %v\n", err)
			return 1
		}
		outFile = f
		w = f
	}
	bw := bufio.NewWriterSize(w, 1<<20)

	m := &manifest{enc: json.NewEncoder(bw), opts: options{hash: *hash, maxHash: *maxHash, infoOnly: *infoOnly}}
	m.enc.SetEscapeHTML(false)
	walkErr := m.walk(flags.Arg(0))

	flushErr := bw.Flush()
	var closeErr error
	if outFile != nil {
		closeErr = outFile.Close()
	}
	if err := errors.Join(m.writeErr, flushErr, closeErr); err != nil {
		fmt.Fprintf(stderr, "dues-parsemanifest: writing manifest: %v\n", err)
		return 1
	}
	if walkErr != nil {
		fmt.Fprintf(stderr, "dues-parsemanifest: %v\n", walkErr)
		return 1
	}
	return 0
}

type manifest struct {
	enc      *json.Encoder
	opts     options
	sum      summaryLine
	writeErr error
}

func (m *manifest) emit(v any) {
	if m.writeErr != nil {
		return
	}
	m.writeErr = m.enc.Encode(v)
}

// walk writes the whole manifest for one image. It returns an error only when
// the image itself cannot be opened; everything below that is recorded inline.
func (m *manifest) walk(path string) error {
	start := time.Now()
	head := imageLine{T: "image", Schema: SchemaVersion, Path: path, Tool: toolVersion(), Started: start.UTC().Format(time.RFC3339)}

	img, err := image.Open(path)
	if err != nil {
		head.Error = err.Error()
		m.emit(head)
		m.sum.Errors++
		m.finish(start)
		return fmt.Errorf("open image: %w", err)
	}
	defer img.Close()

	head.Format = string(img.Format())
	head.Size = img.Size()
	if img.Format() == core.FormatVHD {
		head.VHD = describeVHD(path, img)
	}

	if m.opts.infoOnly {
		m.emit(head)
		return nil
	}

	var table core.PartitionTable
	perr := guard(func() error {
		var e error
		table, e = partition.Parse(img)
		return e
	})
	if perr != nil {
		head.Error = "partition: " + perr.Error()
		m.sum.Errors++
	}
	head.Scheme = string(table.Scheme)
	head.Partitions = len(table.Partitions)
	m.emit(head)

	for _, p := range table.Partitions {
		m.walkPartition(img, p)
	}
	m.finish(start)
	return nil
}

func (m *manifest) finish(start time.Time) {
	m.sum.T = "summary"
	m.sum.Seconds = time.Since(start).Seconds()
	m.emit(m.sum)
}

func (m *manifest) walkPartition(img core.Image, p core.Partition) {
	m.sum.Partitions++
	pl := partitionLine{T: "partition", Index: p.Index, Offset: p.Offset, Size: p.Size, TypeName: p.TypeName, Name: p.Name, Allocated: p.Allocated}
	if !p.Allocated || p.Size <= 0 {
		m.emit(pl)
		return
	}

	section := io.NewSectionReader(img, p.Offset, p.Size)
	var fsys core.Filesystem
	openErr := guard(func() error {
		var e error
		fsys, e = fs.OpenAuto(core.OpenParams{Reader: section, Base: p.Offset, Size: p.Size, ImageSize: img.Size()})
		return e
	})
	if openErr != nil {
		if errors.Is(openErr, fs.ErrUnknownFS) {
			pl.FS = string(core.FSUnknown)
		} else {
			pl.OpenError = openErr.Error()
			if isPanic(openErr) {
				pl.Panic = openErr.Error()
				m.sum.Panics++
			}
			m.sum.Errors++
		}
		m.emit(pl)
		return
	}
	defer fsys.Close()
	pl.FS = string(fsys.Type())

	// The partition line goes out after the walk so it can carry the walk
	// outcome; file lines reference the partition by index and offset.
	walkErr := guard(func() error {
		return fsys.Walk(context.Background(), func(n core.Node) error {
			m.emit(m.describe(img, fsys, p, n))
			return m.writeErr
		})
	})
	if walkErr != nil {
		pl.WalkError = walkErr.Error()
		if isPanic(walkErr) {
			pl.Panic = walkErr.Error()
			m.sum.Panics++
		}
		m.sum.Errors++
	}
	m.emit(pl)
}

func (m *manifest) describe(img core.Image, fsys core.Filesystem, p core.Partition, n core.Node) fileLine {
	fl := fileLine{T: "file", Part: p.Index, PartOffset: p.Offset, Path: n.Path, Kind: kindString(n.Kind), Deleted: n.IsDeleted}
	switch n.Kind {
	case core.KindFile:
		m.sum.Files++
	case core.KindDir:
		m.sum.Dirs++
	default:
		m.sum.Other++
	}

	var info core.FileInfo
	if err := guard(func() error {
		var e error
		info, e = fsys.Stat(n)
		return e
	}); err != nil {
		fl.StatError = err.Error()
		if isPanic(err) {
			fl.StatPanic = err.Error()
			m.sum.Panics++
		}
		m.sum.Errors++
		m.count(fl)
		return fl
	}
	fl.Size = info.Size
	fl.Deleted = info.IsDeleted
	fl.Fragmented = info.IsFragmented
	fl.Inode = info.Inode
	if n.Kind != core.KindFile {
		m.count(fl)
		return fl
	}

	var runs []core.AbsRange
	if err := guard(func() error {
		c, e := fsys.Content(n)
		fl.ContentKind = c.Kind.String()
		fl.Prov = c.Prov.String()
		fl.Located = c.Located
		runs = c.Ranges()
		return e
	}); err != nil {
		fl.RunsError = err.Error()
		if isPanic(err) {
			fl.RunsPanic = err.Error()
			m.sum.Panics++
		}
		m.sum.Errors++
	}
	if len(runs) > 1 {
		fl.Fragmented = true
	}
	fl.Runs = len(runs)
	fl.RunsInImage = true
	var located []core.AbsRange
	for _, r := range runs {
		if r.Sparse {
			fl.SparseRuns++
		} else {
			located = append(located, r)
			if r.Start < 0 || r.End < r.Start || r.End >= img.Size() {
				fl.RunsInImage = false
			}
		}
		fl.RunBytes += r.Len()
	}
	fl.RunsShort = fl.RunBytes < fl.Size
	// Mirrors parser.buildIdxMap as of this baseline: fragmented files are
	// skipped and each located run of the rest is registered as-is.
	if !fl.Fragmented && len(located) > 0 {
		fl.DuesIndexed = true
		for _, r := range located {
			fl.DuesRangeLen += r.Len()
		}
	}

	if streams, err := fsys.Streams(n); err != nil {
		fl.StreamsErr = err.Error()
	} else {
		fl.Streams = len(streams)
	}

	m.hash(img, fsys, n, runs, located, &fl)
	m.count(fl)
	return fl
}

func (m *manifest) count(fl fileLine) {
	if fl.Deleted {
		m.sum.Deleted++
	}
	if fl.Fragmented {
		m.sum.Fragmented++
	}
	if fl.DuesIndexed {
		m.sum.Indexed++
	}
}

func (m *manifest) hash(img core.Image, fsys core.Filesystem, n core.Node, runs, located []core.AbsRange, fl *fileLine) {
	if m.opts.hash == "none" {
		return
	}
	if m.opts.maxHash > 0 && fl.Size > m.opts.maxHash {
		fl.HashSkipped = "size"
		return
	}

	if m.opts.hash == "runs" || m.opts.hash == "both" {
		if fl.RunsInImage && !fl.RunsShort && fl.RunsError == "" {
			if err := guard(func() error {
				sum, e := hashRuns(img, runs, fl.Size)
				fl.SHA256Runs = sum
				return e
			}); err != nil {
				fl.ReadError = err.Error()
				if isPanic(err) {
					fl.ReadPanic = err.Error()
					m.sum.Panics++
				}
				m.sum.Errors++
			}
		}
		if fl.DuesIndexed && fl.RunsInImage {
			if err := guard(func() error {
				sum, e := hashRuns(img, located, -1)
				fl.SHA256Dues = sum
				return e
			}); err != nil && fl.ReadError == "" {
				fl.ReadError = err.Error()
				m.sum.Errors++
			}
		}
	}

	// Unavailable content (a stale entry, a deleted inode with no surviving
	// block map) has nothing to open; the adapter says so in content_kind.
	if (m.opts.hash == "open" || m.opts.hash == "both") && fl.ContentKind != "unavailable" {
		if err := guard(func() error {
			rc, e := fsys.Open(n)
			if e != nil {
				return e
			}
			defer rc.Close()
			h := sha256.New()
			// Read one byte past Size so an adapter that returns too much shows
			// up as OpenBytes > Size rather than being silently truncated.
			k, e := io.Copy(h, io.LimitReader(rc, fl.Size+1))
			fl.OpenBytes = k
			if e != nil {
				return e
			}
			fl.SHA256Open = hex.EncodeToString(h.Sum(nil))
			return nil
		}); err != nil {
			fl.OpenError = err.Error()
			if isPanic(err) {
				fl.OpenPanic = err.Error()
				m.sum.Panics++
			}
			m.sum.Errors++
		}
	}
}

// hashRuns digests the concatenation of runs read from img, with sparse runs
// read as zeros. A non-negative limit truncates the stream to that many bytes,
// which is how a file's logical content is cut from its last run's slack.
func hashRuns(img core.Image, runs []core.AbsRange, limit int64) (string, error) {
	h := sha256.New()
	buf := make([]byte, 1<<20)
	var done int64
	for _, r := range runs {
		left := r.Len()
		if limit >= 0 && done+left > limit {
			left = limit - done
		}
		off := r.Start
		for left > 0 {
			k := min(int64(len(buf)), left)
			chunk := buf[:k]
			if r.Sparse {
				clear(chunk)
			} else if got, err := img.ReadAt(chunk, off); got != len(chunk) {
				// io.ReaderAt may return io.EOF alongside a full read, so only
				// a short read is a failure.
				if err == nil {
					err = io.ErrUnexpectedEOF
				}
				return "", fmt.Errorf("read %d bytes at %d: got %d: %w", k, off, got, err)
			}
			h.Write(chunk)
			off += k
			left -= k
			done += k
		}
		if limit >= 0 && done >= limit {
			break
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// describeVHD reports the image's own header facts and the chain img was
// opened with. It reopens the file without parent resolution, so it never
// follows a path recorded in the evidence.
func describeVHD(path string, img core.Image) *vhdInfo {
	var info *vhdInfo
	_ = guard(func() error {
		d, err := libvhdi.OpenFileWith(path, &libvhdi.Options{ParentResolver: libvhdi.NoParentResolution})
		if err != nil {
			info = &vhdInfo{ResolveError: err.Error()}
			return nil
		}
		defer d.Close()
		info = &vhdInfo{
			Differencing: d.IsDifferencing(),
			Parent:       d.ParentFilename(),
			Dirty:        d.IsDirty(),
			LogReplayed:  d.LogReplayed(),
		}
		if c, ok := img.(interface {
			ChainDepth() int
			ChainComplete() bool
		}); ok {
			info.ChainDepth, info.ChainComplete = c.ChainDepth(), c.ChainComplete()
		}
		return nil
	})
	return info
}

// panicError carries a recovered panic so the manifest can tell a library
// crash from an ordinary error.
type panicError struct{ msg string }

func (e *panicError) Error() string { return e.msg }

func isPanic(err error) bool {
	var pe *panicError
	return errors.As(err, &pe)
}

// guard runs fn and turns a panic into a *panicError carrying the panic value
// and the innermost frames of its stack.
func guard(fn func() error) (err error) {
	defer func() {
		if r := recover(); r != nil {
			stack := string(debug.Stack())
			if len(stack) > 2048 {
				stack = stack[:2048]
			}
			err = &panicError{msg: fmt.Sprintf("panic: %v | %s", r, strings.ReplaceAll(stack, "\n", " | "))}
		}
	}()
	return fn()
}

func kindString(k core.NodeKind) string {
	switch k {
	case core.KindFile:
		return "file"
	case core.KindDir:
		return "dir"
	default:
		return "other"
	}
}

func toolVersion() string {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return "unknown"
	}
	rev := ""
	dirty := ""
	for _, s := range bi.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			if s.Value == "true" {
				dirty = "+dirty"
			}
		}
	}
	if len(rev) > 12 {
		rev = rev[:12]
	}
	return bi.Main.Path + "@" + rev + dirty
}

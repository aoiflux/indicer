package graph

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"indicer/lib/graph/schema"
	"indicer/lib/util"

	graphenedisk "github.com/aoiflux/graphene/disk"
)

// signedCommitPayload is the size of a signed batch commit's payload in
// graphene v0.9's log: the count and body checksum, the commit sequence,
// time and actor, then the key ID, the signature's length and an Ed25519
// signature. An unsigned commit's is 32 bytes.
const signedCommitPayload = 8 + 8 + 8 + 8 + 8 + 2 + 64

// Status describes a graph store from its files, without opening it.
type Status struct {
	Dir string
	// Marker is the schema marker, nil when there is none.
	Marker *schema.Marker
	// Image is the compacted image, nil when there is none.
	Image *graphenedisk.CSRInfo
	Log   LogStatus
	// Segments are the retired log segments, oldest first, and ChainErr says
	// where they do not follow one another.
	Segments []graphenedisk.SegmentInfo
	ChainErr error
	// ImageErr is set when the store was compacted and its image is gone: an
	// open refuses it.
	ImageErr error
}

// LogStatus describes the live log.
type LogStatus struct {
	Bytes int64
	// Segment is the log's segment sequence number: how many compactions it
	// has been retired by.
	Segment uint64
	Records int
	Commits int
	// Signed are the commits that carry a signature.
	Signed int
	// Loose are records outside any batch, which no commit signs.
	Loose int
	// Truncated is set when the log ends in a record a crash cut short.
	Truncated bool
}

// SegmentBytes is the size of the retired segments together.
func (s Status) SegmentBytes() int64 {
	var n int64
	for _, seg := range s.Segments {
		n += seg.Bytes
	}
	return n
}

// Inspect describes root's graph store. It takes the store's shared lock, so
// it waits for no reader and is refused while a writer has the store, and
// reads the files directly: nothing is replayed, and nothing is written. A
// root without a store is ErrNoGraph.
func Inspect(root string) (Status, error) {
	dir := util.GraphPath(root)
	st := Status{Dir: dir}
	info, err := os.Stat(dir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return st, fmt.Errorf("%w at %s", ErrNoGraph, dir)
	case err != nil:
		return st, fmt.Errorf("graph: %w", err)
	case !info.IsDir():
		return st, fmt.Errorf("graph: %s is not a directory", dir)
	}
	lock, err := acquireLock(dir, true)
	if err != nil {
		return st, err
	}
	defer func() { _ = lock.release() }()

	m, found, err := schema.ReadMarker(dir)
	if err != nil {
		return st, err
	}
	if found {
		st.Marker = &m
	}
	if _, err := os.Stat(filepath.Join(dir, ImageFileName)); err == nil {
		img, err := graphenedisk.InspectCSR(filepath.Join(dir, ImageFileName))
		if err != nil {
			return st, fmt.Errorf("graph: %w", err)
		}
		st.Image = &img
	} else if err := checkImage(dir); err != nil {
		st.ImageErr = err
	}
	if st.Log, err = inspectLog(dir); err != nil {
		return st, err
	}
	if st.Segments, err = graphenedisk.ListSegments(dir); err != nil {
		return st, fmt.Errorf("graph: %w", err)
	}
	st.ChainErr = graphenedisk.VerifySegmentChain(st.Segments)
	return st, nil
}

func inspectLog(dir string) (LogStatus, error) {
	var ls LogStatus
	path := filepath.Join(dir, logFileName)
	if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
		return ls, nil
	}
	seq, err := logSegmentSeq(dir)
	if err != nil {
		return ls, err
	}
	wal, err := graphenedisk.InspectWAL(path)
	if err != nil {
		return ls, fmt.Errorf("graph: %w", err)
	}
	ls.Bytes, ls.Segment, ls.Records, ls.Truncated = wal.FileBytes, seq, len(wal.Records), wal.Truncated
	ls.Commits = len(wal.Commits)
	for _, r := range wal.Records {
		switch r.TypeName {
		case "batch-commit":
			if r.Length >= signedCommitPayload {
				ls.Signed++
			}
		case "batch-begin", "checkpoint":
		default:
			if !r.InBatch {
				ls.Loose++
			}
		}
	}
	return ls, nil
}

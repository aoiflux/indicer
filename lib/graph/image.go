package graph

import (
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"indicer/lib/graph/schema"
	"indicer/lib/logging"

	graphenedisk "github.com/aoiflux/graphene/disk"
	"go.uber.org/zap"
)

// The files graphene keeps a store in.
const (
	// ImageFileName is the compacted image. A compaction folds the log into
	// it, after which the log holds only what came later.
	ImageFileName = "graphene.csr"
	logFileName   = "graphene.wal"

	// A compaction that keeps its log retires it as graphene.NNNNNN.wal.
	segmentPrefix = "graphene."
	segmentSuffix = ".wal"
)

// The live log's header, which graphene v0.9 writes: "GWAL", a framing
// version, the log's segment sequence number, the digest of the segment
// before it, and a CRC-32 of the rest. A log without the magic is the
// headerless format of earlier releases, whose sequence is 0.
const (
	logMagic         = "GWAL"
	logSeqOffset     = 6
	logHeaderCRCAt   = 46
	logHeaderSize    = 50
	scratchExtension = ".tmp"
)

// ErrImageMissing is returned when a store was compacted and its image is
// gone, so the log alone would read as a smaller graph and hand out node IDs
// that are already in use.
var ErrImageMissing = errors.New("graph: the store's compacted image is missing")

// IsScratchFile reports whether name, a file in a graph directory, is one
// graphene is still building, such as the image a compaction writes before
// renaming it into place. It is no part of the store, and a copy leaves it
// out, as graphene's own backup does.
func IsScratchFile(name string) bool { return strings.HasSuffix(name, scratchExtension) }

// checkImage refuses a store whose image a compaction made and something
// later removed.
func checkImage(dir string) error {
	switch _, err := os.Stat(filepath.Join(dir, ImageFileName)); {
	case err == nil:
		return nil
	case !errors.Is(err, fs.ErrNotExist):
		return fmt.Errorf("graph: %w", err)
	}
	why, err := imageExpected(dir)
	if err != nil || why == "" {
		return err
	}
	return fmt.Errorf("%w: %s has no %s, but %s, so its log holds only what came after "+
		"a compaction. The graph is derived from the database: move %s aside and run "+
		"`dues enrich` or `dues micro` to build it again", ErrImageMissing, dir, ImageFileName, why, dir)
}

// imageExpected says why dir should hold an image, or "" when nothing says
// it was ever compacted: the marker records an image, the live log's header
// numbers it after a retired segment, or a retired segment is there.
func imageExpected(dir string) (string, error) {
	m, found, err := schema.ReadMarker(dir)
	if err != nil {
		return "", err
	}
	if found && m.Image != nil {
		return "its schema marker records one", nil
	}
	seq, err := logSegmentSeq(dir)
	if err != nil {
		return "", err
	}
	if seq > 0 {
		return fmt.Sprintf("its log is segment %d", seq), nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return "", fmt.Errorf("graph: %w", err)
	}
	for _, e := range entries {
		if isSegmentName(e.Name()) {
			return "retired log segment " + e.Name() + " is there", nil
		}
	}
	return "", nil
}

// logSegmentSeq reads the segment sequence number from the live log's
// header. A missing log, a headerless one, and a header that fails its
// checksum all read as 0: graphene refuses the last itself, with a better
// message than this could give.
func logSegmentSeq(dir string) (uint64, error) {
	f, err := os.Open(filepath.Join(dir, logFileName))
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("graph: %w", err)
	}
	defer f.Close()
	buf := make([]byte, logHeaderSize)
	n, err := io.ReadFull(f, buf)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return 0, fmt.Errorf("graph: read %s: %w", logFileName, err)
	}
	if n < logHeaderSize || string(buf[:len(logMagic)]) != logMagic ||
		binary.LittleEndian.Uint32(buf[logHeaderCRCAt:]) != crc32.ChecksumIEEE(buf[:logHeaderCRCAt]) {
		return 0, nil
	}
	return binary.LittleEndian.Uint64(buf[logSeqOffset:]), nil
}

// isSegmentName reports whether name is a retired log segment's:
// graphene.NNNNNN.wal.
func isSegmentName(name string) bool {
	mid, ok := strings.CutPrefix(name, segmentPrefix)
	if !ok {
		return false
	}
	mid, ok = strings.CutSuffix(mid, segmentSuffix)
	if !ok || mid == "" {
		return false
	}
	for _, c := range mid {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// adoptImage returns the image record the marker should hold, and whether
// it differs from have. The marker is written after graphene commits a
// compaction, so a crash between the two leaves an image the marker does
// not describe, or describes as it was before; the next writable open
// records it. The image is inspected only then, since that reads all of it.
func adoptImage(dir string, have *schema.ImageRecord) (*schema.ImageRecord, bool) {
	path := filepath.Join(dir, ImageFileName)
	stat, err := os.Stat(path)
	if err != nil {
		return have, false
	}
	seq, err := logSegmentSeq(dir)
	if err != nil || (have != nil && have.SegmentSeq == seq) {
		return have, false
	}
	info, err := graphenedisk.InspectCSR(path)
	if err != nil {
		logging.GetLogger().Warn("graph: the image is not recorded in the schema marker",
			zap.String("path", path), zap.Error(err))
		return have, false
	}
	rec := &schema.ImageRecord{
		CSRVersion:  info.Version,
		CompactedAt: stat.ModTime().UTC(),
		Nodes:       info.NodeCount,
		Edges:       info.EdgeCount,
		SegmentSeq:  seq,
	}
	if info.LastCompactUnixNano != 0 {
		rec.CompactedAt = time.Unix(0, info.LastCompactUnixNano).UTC()
	}
	if info.HasSnapshotRoots {
		rec.SnapshotRoot = hex.EncodeToString(info.SnapshotRoot[:])
	}
	return rec, true
}

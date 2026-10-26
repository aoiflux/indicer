package store

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"io"
	"testing"

	"indicer/internal/console"
	"indicer/internal/testutil/testdb"
	"indicer/lib/cnst"
	"indicer/lib/dbio"
	"indicer/lib/util"
)

func BenchmarkRestoreDataPath(b *testing.B) {
	benchmarks := []struct {
		name   string
		chunks int
		start  int64
	}{
		{name: "start_zero/chunks_16", chunks: 16, start: 0},
		{name: "start_mid/chunks_16", chunks: 16, start: cnst.ChonkSize / 2},
		{name: "start_zero/chunks_64", chunks: 64, start: 0},
		{name: "start_mid/chunks_64", chunks: 64, start: cnst.ChonkSize / 2},
	}

	// Restore's progress bar would land on the benchmark's result lines,
	// which benchstat then cannot read.
	defer console.Quiet()()

	for _, bm := range benchmarks {
		b.Run(bm.name, func(b *testing.B) {
			l := setupRestoreBenchmarkDataset(b, bm.chunks, bm.start)

			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				report, err := restoreLogical(context.Background(), l, io.Discard, RestoreOptions{}, nil)
				if err != nil {
					b.Fatalf("restore: %v", err)
				}
				if report.Size != l.Size() {
					b.Fatalf("unexpected restore size: got %d want %d", report.Size, l.Size())
				}
			}
		})
	}
}

// setupRestoreBenchmarkDataset stores chunks distinct chunks as an evidence
// and returns a reader for it, or, when start is inside it, for the
// partition from start to its end.
func setupRestoreBenchmarkDataset(tb testing.TB, chunks int, start int64) *dbio.Logical {
	tb.Helper()
	db := testdb.Open(tb, testdb.Default)

	cs := cnst.ChonkSize
	content := make([]byte, int64(chunks)*cs)
	for i := range content {
		content[i] = byte(i % 251)
	}
	for i := 0; i < chunks; i++ {
		binary.BigEndian.PutUint64(content[int64(i)*cs:], uint64(i+101))
	}
	rid := seedCompletedEvidence(tb, db.DB, "restore-benchmark", content, "")

	fid := util.AppendToBytesSlice(cnst.EviFileNamespace, rid)
	if start > 0 && start < int64(len(content)) {
		raw := seedPartition(tb, db.DB, rid, "benchmark-part", start, int64(len(content))-start, "")
		key, err := base64.StdEncoding.DecodeString(raw)
		if err != nil {
			tb.Fatal(err)
		}
		fid = util.AppendToBytesSlice(cnst.PartiFileNamespace, key)
	}
	l, err := dbio.OpenLogical(db.DB, fid, dbio.ReadOptions{})
	if err != nil {
		tb.Fatal(err)
	}
	return l
}

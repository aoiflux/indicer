package store

import (
	"testing"

	"indicer/lib/dbio"
	"indicer/lib/structs"

	"github.com/dgraph-io/badger/v4"
)

func BenchmarkInspectEvidenceRepairs(b *testing.B) {
	benchmarks := []struct {
		name         string
		total        int
		pendingEvery int
		failedEvery  int
	}{
		{name: "small_mostly_completed", total: 500, pendingEvery: 10, failedEvery: 20},
		{name: "medium_mixed", total: 2000, pendingEvery: 4, failedEvery: 7},
		{name: "large_mixed", total: 5000, pendingEvery: 3, failedEvery: 5},
	}

	for _, bm := range benchmarks {
		bm := bm
		b.Run(bm.name, func(b *testing.B) {
			db := openTestDB(b)
			seedRepairBenchmarkData(b, db, bm.total, bm.pendingEvery, bm.failedEvery)

			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				report, err := InspectEvidenceRepairs(db, false)
				if err != nil {
					b.Fatalf("InspectEvidenceRepairs: %v", err)
				}
				if report.TotalProblematic <= 0 {
					b.Fatalf("expected problematic evidence in benchmark dataset")
				}
			}
		})
	}
}

func seedRepairBenchmarkData(tb testing.TB, db *badger.DB, total, pendingEvery, failedEvery int) {
	tb.Helper()

	if pendingEvery <= 0 {
		pendingEvery = total + 1
	}
	if failedEvery <= 0 {
		failedEvery = total + 1
	}

	for i := 0; i < total; i++ {
		hash := make([]byte, 32)
		hash[0] = byte(i)
		hash[1] = byte(i >> 8)
		hash[2] = byte(i >> 16)
		hash[3] = byte(i >> 24)

		infile := newTestInputFile(db, "bench-evidence", hash)
		evidenceFile := structs.NewEvidenceFile(infile.GetName(), infile.GetStartIndex(), infile.GetSize(), infile.GetInternalObjects(), "dd", "")

		if i%pendingEvery == 0 {
			evidenceFile.Completed = false
			evidenceFile.Failed = false
		} else if i%failedEvery == 0 {
			evidenceFile.Completed = false
			evidenceFile.Failed = true
		} else {
			evidenceFile.Completed = true
			evidenceFile.Failed = false
		}

		if err := dbio.SetFile(infile.GetID(), evidenceFile, db); err != nil {
			tb.Fatalf("seed evidence %d: %v", i, err)
		}
	}
}

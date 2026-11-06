package dbio_test

import (
	"fmt"
	"testing"

	"indicer/internal/testutil/testdb"
	"indicer/lib/dbio"
)

// TestLSMStatsReadsTheTree: a sample of a database with keys written reads
// without negative counts, and as one log field each.
func TestLSMStatsReadsTheTree(t *testing.T) {
	d := testdb.Open(t, testdb.Default)
	wb := d.DB.NewWriteBatch()
	for i := range 2000 {
		if err := wb.Set(fmt.Appendf(nil, "lsm-%05d", i), make([]byte, 512)); err != nil {
			t.Fatal(err)
		}
	}
	if err := wb.Flush(); err != nil {
		t.Fatal(err)
	}
	s := dbio.LSMStats(d.DB)
	if s.L0Tables < 0 || s.L0Bytes < 0 || s.LSMBytes < 0 || s.VlogBytes < 0 || s.Compacting < 0 || s.CompactedBytes < 0 || s.L0WrittenBytes < 0 {
		t.Fatalf("negative sample: %+v", s)
	}
	if got := len(s.Fields("p_")); got != 7 {
		t.Fatalf("%d fields, want 7", got)
	}
}

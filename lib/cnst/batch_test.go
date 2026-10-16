package cnst

import "testing"

// TestGetMaxBatchCountBounded: the pending-transaction limit of a write batch
// must be at least 1 (0 deadlocks badger) and at most MaxPendingBatchTxns
// (each pending transaction holds memory), whatever the chunk size (CFG-5).
func TestGetMaxBatchCountBounded(t *testing.T) {
	prevSize, prevLow := ChonkSize, MEMOPT
	t.Cleanup(func() { ChonkSize, MEMOPT = prevSize, prevLow })

	for _, low := range []bool{false, true} {
		for _, size := range []int64{0, -1, 4 * KB, 256 * KB, 64 * MB, 1 << 50} {
			ChonkSize, MEMOPT = size, low
			n, err := GetMaxBatchCount()
			if err != nil {
				t.Fatalf("chunk %d low %v: %v", size, low, err)
			}
			if n < 1 || n > MaxPendingBatchTxns {
				t.Errorf("chunk %d low %v: %d pending transactions, want 1..%d", size, low, n, MaxPendingBatchTxns)
			}
		}
	}
}

package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"indicer/lib/cnst"
)

func TestStoreDataSmallFileWithIndexingAcrossHashStrategies(t *testing.T) {
	t.Parallel()
	const chunkKB = 256

	inputPath := filepath.Join(t.TempDir(), "tiny.bin")
	if err := os.WriteFile(inputPath, bytes.Repeat([]byte("tiny-file-race-check"), 32), 0o600); err != nil {
		t.Fatalf("write tiny input file: %v", err)
	}

	key := []byte("01234567890123456789012345678901")
	strategies := []string{cnst.SyncHashStrategy, cnst.AsyncHashStrategy, cnst.HochoHashStrategy}
	for _, strategy := range strategies {
		strategy := strategy
		t.Run(strategy, func(t *testing.T) {
			tun := cnst.DefaultTuning()
			tun.HashStrategy = strategy
			dbPath := filepath.Join(t.TempDir(), "db-"+strategy)
			opts := optsFor(dbPath, chunkKB, key)
			opts.Tuning = &tun
			if err := StoreData(t.Context(), opts, inputPath, false, false, false, false); err != nil {
				t.Fatalf("StoreData %s small file: %v", strategy, err)
			}
		})
	}
}

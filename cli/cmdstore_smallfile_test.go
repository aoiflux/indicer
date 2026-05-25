package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"indicer/lib/cnst"
	"indicer/lib/util"
)

func TestStoreDataSmallFileWithIndexingAcrossHashStrategies(t *testing.T) {
	ensureZstdReadyForTests(t)

	originalStrategy := cnst.StoreHashStrategy
	originalHochoMode := cnst.HochoMode
	originalChunkSize := cnst.ChonkSize
	t.Cleanup(func() {
		cnst.StoreHashStrategy = originalStrategy
		cnst.HochoMode = originalHochoMode
		cnst.ChonkSize = originalChunkSize
	})

	const chunkKB = 256
	util.SetChonkSize(chunkKB)

	inputPath := filepath.Join(t.TempDir(), "tiny.bin")
	if err := os.WriteFile(inputPath, bytes.Repeat([]byte("tiny-file-race-check"), 32), 0o600); err != nil {
		t.Fatalf("write tiny input file: %v", err)
	}

	key := []byte("01234567890123456789012345678901")
	strategies := []string{cnst.SyncHashStrategy, cnst.AsyncHashStrategy, cnst.HochoHashStrategy}
	for _, strategy := range strategies {
		strategy := strategy
		t.Run(strategy, func(t *testing.T) {
			cnst.StoreHashStrategy = strategy
			cnst.HochoMode = cnst.HochoModeBaseline
			dbPath := filepath.Join(t.TempDir(), "db-"+strategy)
			if err := StoreData(chunkKB, dbPath, inputPath, key, false, false, false, false); err != nil {
				t.Fatalf("StoreData %s small file: %v", strategy, err)
			}
		})
	}
}

package cli

import (
	"context"
	"fmt"
	"indicer/internal/console"
	"indicer/lib/dbopen"
	"indicer/lib/near"
	"indicer/lib/search"
	"indicer/lib/util"
	"indicer/tui"
	"os"
	"strings"
	"sync"
)

var tuiIOGuard sync.Mutex

// runQuiet runs a TUI action with DUES's own terminal output silenced, so it
// cannot draw over the TUI.
func runQuiet(fn func() error) error {
	tuiIOGuard.Lock()
	defer tuiIOGuard.Unlock()
	defer console.Quiet()()
	return fn()
}

// TUICmd launches the interactive TUI interface
func TUICmd(ctx context.Context, opts DBOptions) (err error) {
	h, err := Common(ctx, opts, dbopen.ReadWrite, true)
	if err != nil {
		return err
	}
	db := h.DB
	chonkSize := h.Manifest.Layout.ChunkKiB
	var key []byte // the store path takes its keys from the database

	actions := tui.Actions{
		Store: func(filePath string, syncIndex bool, noIndex bool, hashAlgo string) error {
			return runQuiet(func() error {
				// The chunk hash is part of the database's layout. The TUI's
				// per-store toggle goes in W11; until then it may only
				// confirm the database's own algorithm.
				if want := h.Manifest.Layout.ChunkHash; !strings.EqualFold(hashAlgo, want) {
					return fmt.Errorf("this database uses %s; its hash algorithm cannot change", want)
				}
				if err := util.EnsureBlobPath(db.Opts().Dir); err != nil {
					return err
				}

				info, err := os.Stat(filePath)
				if err != nil {
					return err
				}

				if info.IsDir() {
					return StoreFolder(chonkSize, filePath, key, syncIndex, noIndex, false, false, db)
				}

				return StoreFile(chonkSize, filePath, key, syncIndex, noIndex, false, false, db)
			})
		},
		Search: func(ctx context.Context, query string) error {
			if len(query) < 2 {
				return fmt.Errorf("query must be at least 2 characters")
			}
			return runQuiet(func() error {
				return search.SearchWithContext(ctx, strings.ToLower(query), db)
			})
		},
		Restore: func(hash string, restorePath string) error {
			// The TUI replaces an existing file, as it always has, but only
			// once the restore has succeeded (TUI-2).
			return runQuiet(func() error {
				return restoreToPath(ctx, db, hash, restorePath, true)
			})
		},
		NearIn: func(hash string, deep bool) error {
			return runQuiet(func() error {
				return near.NearInFile(hash, db, deep, false, 0)
			})
		},
		NearOut: func(filePath string) error {
			return runQuiet(func() error {
				return near.NearOutFile(filePath, db, false, false, false, 0)
			})
		},
		Reset: func() error {
			// The same entries the reset command deletes, with the database
			// kept open: the graph and full-text index go too (ING-16).
			return runQuiet(h.Reset)
		},
	}

	err = tui.RunTUI(db, actions)
	closeDB(h, &err)
	return err
}

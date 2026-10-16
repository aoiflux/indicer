package cli

import (
	"context"
	"fmt"
	"indicer/internal/console"
	"indicer/lib/cnst"
	"indicer/lib/dbopen"
	"indicer/lib/near"
	"indicer/lib/search"
	"indicer/lib/store"
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

	resetRequested := false
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
			return runQuiet(func() error {
				fhandle, err := os.OpenFile(restorePath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, cnst.FilePerm)
				if err != nil {
					return err
				}
				defer fhandle.Close()
				return store.Restore(hash, fhandle, db)
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
			return runQuiet(func() error {
				if err := db.DropAll(); err != nil {
					return err
				}

				blobDir := util.BlobPath(db.Opts().Dir)
				if err := os.RemoveAll(blobDir); err != nil {
					return err
				}
				if err := util.EnsureBlobPath(db.Opts().Dir); err != nil {
					return err
				}
				return nil
			})
		},
	}

	err = tui.RunTUI(db, actions)
	if !resetRequested {
		closeDB(h, &err)
	}

	return err
}

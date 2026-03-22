package cli

import (
	"fmt"
	"indicer/lib/cnst"
	"indicer/lib/near"
	"indicer/lib/search"
	"indicer/lib/store"
	"indicer/lib/util"
	"indicer/tui"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

var tuiIOGuard sync.Mutex

func runQuiet(fn func() error) error {
	tuiIOGuard.Lock()
	defer tuiIOGuard.Unlock()

	devNull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	defer devNull.Close()

	stdout := os.Stdout
	stderr := os.Stderr
	os.Stdout = devNull
	os.Stderr = devNull
	defer func() {
		os.Stdout = stdout
		os.Stderr = stderr
	}()

	return fn()
}

// TUICmd launches the interactive TUI interface
func TUICmd(chonkSize int, dbpath string, key []byte) error {
	util.SetChonkSize(chonkSize)
	db, _, err := Common(chonkSize, dbpath, key)
	if err != nil {
		return err
	}

	resetRequested := false
	actions := tui.Actions{
		Store: func(filePath string, syncIndex bool, noIndex bool) error {
			return runQuiet(func() error {
				if err := util.EnsureBlobPath(db.Opts().Dir); err != nil {
					return err
				}

				info, err := os.Stat(filePath)
				if err != nil {
					return err
				}

				if info.IsDir() {
					return StoreFolder(chonkSize, filePath, key, syncIndex, noIndex, db)
				}

				return StoreFile(chonkSize, filePath, key, syncIndex, noIndex, db)
			})
		},
		Search: func(query string) error {
			if len(query) < 2 {
				return fmt.Errorf("query must be at least 2 characters")
			}
			return runQuiet(func() error {
				return search.Search(strings.ToLower(query), db)
			})
		},
		Restore: func(hash string, restorePath string) error {
			return runQuiet(func() error {
				fhandle, err := os.Create(restorePath)
				if err != nil {
					return err
				}
				defer fhandle.Close()
				return store.Restore(hash, fhandle, db)
			})
		},
		NearIn: func(hash string, deep bool) error {
			return runQuiet(func() error {
				return near.NearInFile(hash, db, deep)
			})
		},
		NearOut: func(filePath string) error {
			return runQuiet(func() error {
				return near.NearOutFile(filePath, db)
			})
		},
		Reset: func() error {
			return runQuiet(func() error {
				if err := db.DropAll(); err != nil {
					return err
				}

				blobDir := filepath.Join(db.Opts().Dir, cnst.BLOBSDIR)
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
		closeErr := db.Close()
		if err == nil {
			err = closeErr
		}
	}

	return err
}

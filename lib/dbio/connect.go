package dbio

import (
	"os"

	"indicer/lib/cnst"
	"indicer/lib/util"

	"github.com/dgraph-io/badger/v4"
	"github.com/dgraph-io/badger/v4/options"
)

// ConnectDB opens the badger database under datadir without dbopen, as tests
// and tools do, with the default tuning; quick opens a legacy quick-mode one.
func ConnectDB(datadir string, key []byte, quick bool) (*badger.DB, error) {
	return ConnectDBTuned(datadir, key, quick, cnst.DefaultTuning())
}

// ConnectDBTuned is ConnectDB with badger's caches and tables sized by t, as
// a database dbopen opens with that tuning has them.
func ConnectDBTuned(datadir string, key []byte, quick bool, t cnst.Tuning) (*badger.DB, error) {
	datadir = util.KVDBPath(datadir)
	if err := os.MkdirAll(datadir, cnst.DirPerm); err != nil {
		return nil, err
	}
	// Legacy quick mode used badger's default (Snappy) tables and no key.
	if quick {
		return badger.Open(BadgerOptions(datadir, nil, options.Snappy, t))
	}
	return badger.Open(BadgerOptions(datadir, key, options.ZSTD, t))
}

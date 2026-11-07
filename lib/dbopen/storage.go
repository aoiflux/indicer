package dbopen

import (
	"encoding/json"
	"errors"
	"fmt"

	"indicer/lib/cnst"
	"indicer/lib/dbmeta"
	"indicer/lib/engine"
	"indicer/lib/fio"

	"github.com/dgraph-io/badger/v4"
)

// SetStorage changes the storage the manifest records, for `repair
// --upgrade`, which moves a hierarchical database's block index into C|||
// and then records it as a container database. It writes the in-database
// marker and then the manifest file and its mirror. Both storages read the
// same way apart from the block index lookup, so the marker check treats
// them as one, and a crash between the two writes leaves a database that
// opens.
func (h *Handle) SetStorage(storage string) error {
	if h.ReadOnly {
		return errors.New("dbopen: SetStorage needs a writable open")
	}
	m := h.Manifest
	m.Layout.Storage = storage
	if err := m.Validate(); err != nil {
		return err
	}
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	err = h.DB.Update(func(txn *badger.Txn) error {
		return txn.Set([]byte(dbmeta.MarkerKey), b)
	})
	if err != nil {
		return fmt.Errorf("dbopen: record storage %s: %w", storage, err)
	}
	if err := dbmeta.Save(h.Root, m); err != nil {
		return fmt.Errorf("dbopen: record storage %s: %w", storage, err)
	}
	h.Manifest = m
	dbmeta.RegisterManifest(h.DB, m)
	engine.For(h.DB).SetLayout(m.Layout)
	ApplyToGlobals(m)
	h.keepBlockIndexLookup()
	return nil
}

// keepBlockIndexLookup sets the legacy block index lookup from the manifest,
// and turns it on when block index files exist, whatever the manifest says:
// an upgrade may have kept them, or been cut short before the manifest
// changed.
func (h *Handle) keepBlockIndexLookup() {
	on := h.Manifest.Layout.Storage == dbmeta.StorageContainerBIDX
	if !on {
		files, _ := fio.BlockIndexFiles(h.Root)
		on = len(files) > 0
	}
	engine.For(h.DB).SetBlockIndex(on)
	cnst.LegacyBlockIndex = on
}

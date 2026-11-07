package compression_product

import (
	"context"

	"indicer/internal/ui"
	"indicer/lib/cnst"
	"indicer/lib/dbmeta"
	"indicer/lib/dbopen"
	"indicer/lib/zstdx"
)

// engineSlot admits one operation at a time to the DUES storage engine.
//
// Opening a database sets process-wide layout globals that parts of the
// storage code still read (chunk size, chunk hash, value encoding, storage
// mode). Two archives open at once in one process, as async FFI tasks can
// be, would read each other's layout (FFI-2). Until the engine reads only
// the layout registered for each database, operations take turns.
var engineSlot = make(chan struct{}, 1)

// withStoreRuntime runs fn alone among the operations that use the storage
// engine, with an encoder of its own, and puts the layout globals back as
// they were afterwards. A context that ends while waiting leaves the queue.
func withStoreRuntime(ctx context.Context, fn func() error) error {
	select {
	case engineSlot <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-engineSlot }()
	if err := ctx.Err(); err != nil {
		return err
	}
	dropEncoder, err := useOperationEncoder()
	if err != nil {
		return err
	}
	defer dropEncoder()
	saved := saveLayoutGlobals()
	defer saved.restore()
	return fn()
}

// layoutGlobals are the globals dbopen.ApplyToGlobals sets.
type layoutGlobals struct {
	chunkSize        int64
	hashAlgo         string
	quick            bool
	container        bool
	legacyBlockIndex bool
}

func saveLayoutGlobals() layoutGlobals {
	return layoutGlobals{
		chunkSize:        cnst.ChonkSize,
		hashAlgo:         cnst.HASHALGO,
		quick:            cnst.QUICKOPT,
		container:        cnst.CONTAINERMODE,
		legacyBlockIndex: cnst.LegacyBlockIndex,
	}
}

func (g layoutGlobals) restore() {
	cnst.ChonkSize = g.chunkSize
	cnst.HASHALGO = g.hashAlgo
	cnst.QUICKOPT = g.quick
	cnst.CONTAINERMODE = g.container
	cnst.LegacyBlockIndex = g.legacyBlockIndex
}

// useOperationEncoder acquires an encoder for the running operation as
// cnst.ENCODER and returns the function that puts the previous one back and
// releases it. A library host keeps the process for as long as it runs, and
// an encoder keeps its states (about 50 MB each at the best level) until it
// is dropped, so one kept for the process held them for good after the first
// archive (FFI-4); the last release drops it. The engine slot makes the swap
// safe: no other operation reads the encoder meanwhile.
func useOperationEncoder() (func(), error) {
	enc, err := zstdx.Acquire(cnst.CompressLevel, cnst.GetMaxThreadCount())
	if err != nil {
		return nil, err
	}
	prev := cnst.ENCODER
	cnst.ENCODER = enc
	return func() {
		cnst.ENCODER = prev
		enc.Release()
	}, nil
}

// openArchiveDB opens an archive's embedded DUES database and prints the
// open's notices, as the dues commands do. An empty password means none:
// the database is unencrypted, or a legacy one keyed with the empty
// password. It recovers nothing: one operation writes an archive's
// database, and the others only read it.
func openArchiveDB(ctx context.Context, dbPath, password string, chunkSizeKB int, mode dbopen.Mode) (*dbopen.Handle, error) {
	creds := dbmeta.NoPassword()
	if password != "" {
		creds = dbmeta.Password(password)
	}
	h, err := dbopen.Open(ctx, dbopen.Request{Path: dbPath, Mode: mode, Creds: creds, Layout: dbopen.Requested{ChunkKiB: chunkSizeKB}, WrittenBy: "dues"})
	if err != nil {
		return nil, err
	}
	for _, n := range h.Notices {
		ui.Notef("%s", n)
	}
	return h, nil
}

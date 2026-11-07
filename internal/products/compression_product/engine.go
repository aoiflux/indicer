package compression_product

import (
	"context"

	"indicer/internal/ui"
	"indicer/lib/dbmeta"
	"indicer/lib/dbopen"
)

// engineSlot admits one operation at a time to the DUES storage engine.
//
// Each open database now keeps its own layout, tuning and encoder (package
// engine), and opening one sets nothing process-wide, but operations still
// take turns until the concurrency tests show two archives at once are
// safe (FFI-2).
var engineSlot = make(chan struct{}, 1)

// withStoreRuntime runs fn alone among the operations that use the storage
// engine. A context that ends while waiting leaves the queue.
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
	return fn()
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

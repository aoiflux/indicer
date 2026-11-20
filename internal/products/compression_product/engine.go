package compression_product

import (
	"context"

	"indicer/internal/ui"
	"indicer/lib/dbmeta"
	"indicer/lib/dbopen"
)

// openArchiveDB opens an archive's embedded DUES database and prints the
// open's notices, as the dues commands do. An empty password means none:
// the database is unencrypted, or a legacy one keyed with the empty
// password. It recovers nothing: one operation writes an archive's
// database, and the others only read it.
//
// Operations on different archives run at once (FFI-2): each open database
// keeps its own layout, tuning and encoder (package engine), and opening one
// sets nothing process-wide.
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

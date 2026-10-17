package cli

import (
	"context"

	"indicer/internal/testutil/testdb"
	"indicer/lib/dbmeta"
	"indicer/lib/dbopen"
)

// testKDF keeps password-protected test databases fast to open.
var testKDF = dbmeta.KDFParams{Time: 1, MemoryKiB: 64, Threads: 1}

func init() { dbmeta.DefaultKDF = testKDF }

// optsFor returns the options a test stores and restores with. password may
// be empty for an unencrypted database.
func optsFor(dbPath string, chunkKB int, password []byte) DBOptions {
	creds := dbmeta.NoPassword()
	if len(password) > 0 {
		creds = dbmeta.Password(string(password))
	}
	return DBOptions{Path: dbPath, Creds: creds, Layout: dbopen.Requested{ChunkKiB: chunkKB}}
}

// optsForMode returns the options for one testdb storage mode.
func optsForMode(dbPath string, chunkKB int, m testdb.Mode) DBOptions {
	o := optsFor(dbPath, chunkKB, []byte(m.Password))
	o.Layout.Container = m.Container
	o.Layout.Quick = m.Quick
	return o
}

// openTestDB opens a database a test stored into, for direct inspection.
// The caller must Close it before the next command opens the database.
func openTestDB(dbPath string, password []byte) (*dbopen.Handle, error) {
	o := optsFor(dbPath, 0, password)
	return dbopen.Open(context.Background(), dbopen.Request{Path: o.Path, Mode: dbopen.ReadWrite, Creds: o.Creds})
}

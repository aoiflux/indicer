package tui

import (
	"context"
	"errors"

	"indicer/lib/microartefact"
	"indicer/lib/store"
)

// Info describes the open database. The TUI shows it and never changes it:
// the chunk size and hash are part of the database's layout (CFG-6).
type Info struct {
	Root      string
	ChunkKiB  int
	ChunkHash string
	Encrypted bool
}

// Actions are what the TUI does to the database. Each runs as a job, with a
// context that ends when the job is cancelled or the TUI stops. The tui
// package itself never reads the database.
type Actions struct {
	// Store stores the file or folder at path.
	Store func(ctx context.Context, path string, opts StoreOptions) (Outcome, error)
	// Search searches for query and writes the report.
	Search func(ctx context.Context, query string) (Outcome, error)
	// Restore restores the object id (base64) to path. An existing file is
	// replaced only with replace; without it the error is ErrTargetExists.
	Restore func(ctx context.Context, id, path string, replace bool) (Outcome, error)
	// NearIn and NearOut find what shares content with a stored object, or
	// with a file outside the database, and write the report.
	NearIn  func(ctx context.Context, id string, deep bool) (Outcome, error)
	NearOut func(ctx context.Context, path string) (Outcome, error)
	// Reset empties the database.
	Reset func(ctx context.Context) error
	// List lists at most limit completed evidence, and how many there are.
	List func(ctx context.Context, limit int) (EvidenceList, error)
	// Stats gathers the database's statistics.
	Stats func(ctx context.Context) (*store.DBStats, error)
	// Micro reads the micro-artefact view: the files with the most
	// artefacts, under their partitions and evidence.
	Micro func(ctx context.Context) (MicroView, error)
}

// StoreOptions are the store screen's options.
type StoreOptions struct {
	NoIndex bool
}

// Outcome is what a finished action did, line by line. Warning marks an
// action that finished but needs the examiner's attention: what the command
// line reports with exit code 2. A failed action may still report what it
// did before it failed.
type Outcome struct {
	Lines   []string
	Warning bool
}

// ErrTargetExists means a restore would replace an existing file.
var ErrTargetExists = errors.New("the restore target exists")

// EvidenceList is the list screen's evidence: at most the limit asked for,
// and the number stored.
type EvidenceList struct {
	Rows  []EvidenceRow
	Total int
}

// EvidenceRow is one stored evidence.
type EvidenceRow struct {
	// ID is the evidence's ID in base64: what restore and NeAR take.
	ID     string
	Name   string
	Size   int64
	Format string
	// Index is the evidence's index state, or "" for a legacy record.
	Index string
}

// MicroView is what the micro-artefact screen shows.
type MicroView struct {
	// Tree holds the files shown, under their partitions and evidence.
	Tree *microartefact.EvidenceFileHierarchy
	// Files and Artefacts count every file with artefacts, and their
	// artefacts, whether shown or not.
	Files, Artefacts int
	// Shown is how many files Tree holds; PerFile the most artefacts shown
	// for one file.
	Shown, PerFile int
}

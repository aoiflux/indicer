package cli

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"indicer/internal/lifecycle"
	"indicer/lib/dbmeta"
)

// TestResetDataConfirmation is the ING-16 regression for the command: one
// "y" deleted any non-empty directory, and a script piping "y" could do so
// unattended.
func TestResetDataConfirmation(t *testing.T) {
	newDB := func(t *testing.T) string {
		dbPath := filepath.Join(t.TempDir(), "caseA")
		h, err := openTestDB(dbPath, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := h.Close(); err != nil {
			t.Fatal(err)
		}
		return dbPath
	}

	t.Run("y is not enough", func(t *testing.T) {
		db := newDB(t)
		if err := resetData(db, false, strings.NewReader("y\n"), true); err != nil {
			t.Fatal(err)
		}
		if !dbmeta.HasDatabase(db) {
			t.Fatal(`"y" deleted the database`)
		}
	})
	t.Run("typed name deletes", func(t *testing.T) {
		db := newDB(t)
		if err := resetData(db, false, strings.NewReader("caseA\n"), true); err != nil {
			t.Fatal(err)
		}
		if dbmeta.HasDatabase(db) {
			t.Fatal("confirmed reset kept the database")
		}
	})
	t.Run("script needs yes", func(t *testing.T) {
		db := newDB(t)
		err := resetData(db, false, strings.NewReader("caseA\n"), false)
		var usage *lifecycle.UsageError
		if !errors.As(err, &usage) {
			t.Fatalf("non-interactive reset without --yes = %v, want a usage error", err)
		}
		if !dbmeta.HasDatabase(db) {
			t.Fatal("refused reset deleted the database")
		}
		if err := resetData(db, true, strings.NewReader(""), false); err != nil {
			t.Fatal(err)
		}
		if dbmeta.HasDatabase(db) {
			t.Fatal("--yes reset kept the database")
		}
	})
	t.Run("not a database", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "thesis.doc"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		err := resetData(dir, true, strings.NewReader(""), false)
		var usage *lifecycle.UsageError
		if !errors.As(err, &usage) {
			t.Fatalf("reset of a non-DUES directory = %v, want a usage error", err)
		}
		if _, err := os.Stat(filepath.Join(dir, "thesis.doc")); err != nil {
			t.Fatal("reset deleted a file in a non-DUES directory")
		}
	})
	t.Run("missing path", func(t *testing.T) {
		if err := resetData(filepath.Join(t.TempDir(), "absent"), true, strings.NewReader(""), false); err != nil {
			t.Fatalf("reset of a missing path = %v, want nothing to do", err)
		}
	})
}

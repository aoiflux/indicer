package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"indicer/internal/lifecycle"
	"indicer/internal/testutil"
	"indicer/internal/testutil/testdb"
	"indicer/lib/store"
)

// TestRestoreTargetSafety is the RST-4 regression. Restore replaced an
// existing target without asking, deleted an unrelated <target>.bak to make
// room for its backup, moved a directory target aside, and left its temp
// file behind when interrupted.
func TestRestoreTargetSafety(t *testing.T) {
	testdb.Isolate(t)
	testdb.EnsureCodecs(t)
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "db")
	opts := optsFor(dbPath, matrixChunkKB, nil)
	src := testutil.WriteRandomFile(t, dir, "evidence.bin", 21, 3*matrixChunkKB*1024+9)
	if err := StoreData(t.Context(), opts, src, false, true, false, false); err != nil {
		t.Fatalf("store: %v", err)
	}
	hash := plainHashB64(t, src)
	want := testutil.FileSHA256(t, src)

	out := t.TempDir()
	target := filepath.Join(out, "restored.bin")
	write := func(p, s string) {
		t.Helper()
		if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	read := func(p string) string {
		t.Helper()
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	noTemps := func() {
		t.Helper()
		entries, _ := os.ReadDir(out)
		for _, e := range entries {
			if strings.Contains(e.Name(), "tmp") {
				t.Fatalf("restore left %s behind", e.Name())
			}
		}
	}

	write(target, "existing evidence copy")
	write(target+".bak", "a user's own backup")

	err := RestoreData(t.Context(), opts, hash, target, false)
	var usage *lifecycle.UsageError
	if !errors.As(err, &usage) {
		t.Fatalf("restore over an existing file without --force = %v, want a usage error", err)
	}
	if read(target) != "existing evidence copy" {
		t.Fatal("refused restore changed the target")
	}

	if err := RestoreData(t.Context(), opts, "bm90LWEtcmVhbC1oYXNo", target, true); err == nil {
		t.Fatal("restore of an unknown hash succeeded")
	}
	if read(target) != "existing evidence copy" {
		t.Fatal("failed restore changed the target")
	}
	noTemps()

	if err := RestoreData(t.Context(), opts, hash, target, true); err != nil {
		t.Fatalf("restore --force: %v", err)
	}
	if testutil.FileSHA256(t, target) != want {
		t.Fatal("restore --force wrote the wrong content")
	}
	if read(target+".bak") != "a user's own backup" {
		t.Fatal("restore touched <target>.bak, which it does not own")
	}
	noTemps()

	sub := filepath.Join(out, "adir")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	write(filepath.Join(sub, "inside.txt"), "x")
	if err := RestoreData(t.Context(), opts, hash, sub, true); err == nil {
		t.Fatal("restore onto a directory succeeded")
	}
	if read(filepath.Join(sub, "inside.txt")) != "x" {
		t.Fatal("restore onto a directory disturbed it")
	}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	// The restore pipeline itself stops on a cancelled context.
	h, err := openTestDB(dbPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(filepath.Join(out, "direct.bin"))
	if err != nil {
		t.Fatal(err)
	}
	rerr := store.Restore(ctx, hash, f, h.DB)
	f.Close()
	h.Close()
	if !errors.Is(rerr, context.Canceled) {
		t.Fatalf("store.Restore with a cancelled context = %v, want context.Canceled", rerr)
	}
	if err := os.Remove(filepath.Join(out, "direct.bin")); err != nil {
		t.Fatal(err)
	}

	fresh := filepath.Join(out, "cancelled.bin")
	if err := RestoreData(ctx, opts, hash, fresh, false); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled restore = %v, want context.Canceled", err)
	}
	if _, err := os.Stat(fresh); !os.IsNotExist(err) {
		t.Fatalf("cancelled restore created its target: %v", err)
	}
	noTemps()
}

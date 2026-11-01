package cli

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"indicer/internal/lifecycle"
	"indicer/internal/testutil"
	"indicer/internal/testutil/testdb"
	"indicer/lib/cnst"
)

// TestRepairVerify covers `dues repair --verify` (invariant 7): intact
// evidence verifies, and a flipped bit in one stored chunk is reported and
// fails the command, while the other evidence still verifies. The document
// on stdout holds the results either way.
func TestRepairVerify(t *testing.T) {
	testdb.ForEachMode(t, func(t *testing.T, m testdb.Mode) {
		dir := t.TempDir()
		dbPath := filepath.Join(dir, "db")
		opts := optsForMode(dbPath, matrixChunkKB, m)
		for i, n := range []int{2*matrixChunkKB*1024 + 3, matrixChunkKB * 1024} {
			p := testutil.WriteRandomFile(t, dir, "in/obj"+string(rune('a'+i))+".bin", uint64(500+i), n)
			if err := StoreData(t.Context(), opts, p, false, true, false, false); err != nil {
				t.Fatal(err)
			}
		}
		verify := func(want ...string) (verifyDoc, string, error) {
			doc, stderr, err := runRepair(t, optsFor(dbPath, 0, []byte(m.Password)), RepairOptions{Verify: true}, "", false, want...)
			return verifySection(t, doc), stderr, err
		}
		v, stderr, err := verify("verify")
		if err != nil {
			t.Fatalf("verify of intact evidence: %v", err)
		}
		if v.Verified != 2 || v.Damaged != 0 || len(v.Results) != 2 || !strings.Contains(stderr, "Verified 2 evidence; 0 damaged.\n") {
			t.Fatalf("verify of intact evidence: %+v\nstderr:\n%s", v, stderr)
		}

		// Damage the newest blob or container: one chunk of one evidence.
		blobs, err := filepath.Glob(filepath.Join(dbPath, cnst.BLOBSDIR, "*"+cnst.BLOBEXT))
		if err != nil || len(blobs) == 0 {
			t.Fatalf("no blobs: %v", err)
		}
		target := blobs[0]
		raw, err := os.ReadFile(target)
		if err != nil {
			t.Fatal(err)
		}
		raw[len(raw)-10] ^= 0x10
		if err := os.WriteFile(target, raw, 0o644); err != nil {
			t.Fatal(err)
		}
		// The command fails, and its document still holds every result,
		// the damaged one with its problem, and the error.
		v, stderr, err = verify("verify", "error")
		var verr *VerifyError
		if !errors.As(err, &verr) || verr.Problems != 1 || verr.Verified != 1 || lifecycle.ExitCode(err) != 1 {
			t.Fatalf("verify after flipping a bit in %s: %v", filepath.Base(target), err)
		}
		damaged := 0
		for _, r := range v.Results {
			if r.Problem != "" {
				damaged++
			}
		}
		if v.Verified != 1 || v.Damaged != 1 || len(v.Results) != 2 || damaged != 1 || !strings.Contains(stderr, "DAMAGED") {
			t.Fatalf("verify after flipping a bit: %+v\nstderr:\n%s", v, stderr)
		}
	})
}

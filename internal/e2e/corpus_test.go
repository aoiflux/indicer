package e2e

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"indicer/internal/diskspace"
	"indicer/internal/testutil"
	"indicer/lib/dbopen"
	"indicer/lib/evidence"
	"indicer/lib/store"
	"indicer/lib/structs"
)

// The real-image corpus, which only some machines have:
//
//	go test ./internal/e2e -run TestCorpusRoundTrip -args -dues.corpus-list=FILE -dues.corpus-scratch=DIR
var (
	corpusList    = flag.String("dues.corpus-list", "", "a file naming one evidence image per line (TestCorpusRoundTrip)")
	corpusScratch = testutil.NewDirFlag("dues.corpus-scratch", "a directory with room for the corpus round trip's databases")
)

// TestCorpusRoundTrip stores each image of the real corpus (one path per
// line of the -dues.corpus-list file) in a fresh database under
// -dues.corpus-scratch, restores it with full verification into a hash,
// and compares that with a digest that does not come from DUES's ingest:
//
//   - a raw image: the SHA-256 of the file itself;
//   - an E01 set: the MD5 and SHA-1 the set recorded at acquisition;
//   - a VHD/VHDX: only the hash its ingest recorded over the decoded disk,
//     since DUES's decoder produced both (storage is checked; decoding was
//     checked against libyal's vhdimount in W1 and W5).
//
// An image whose files would not fit the scratch volume is skipped and
// named. A differencing disk whose ancestor is missing must be refused.
func TestCorpusRoundTrip(t *testing.T) {
	t.Parallel()
	list := *corpusList
	if list == "" {
		t.Skip("pass -args -dues.corpus-list=FILE to run it")
	}
	scratch := corpusScratch.Require(t)
	bin := testutil.BuildDues(t)
	f, err := os.Open(list)
	if err != nil {
		t.Fatal(err)
	}
	var images []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if p := strings.TrimSpace(sc.Text()); p != "" && !strings.HasPrefix(p, "#") {
			images = append(images, p)
		}
	}
	f.Close()

	for _, img := range images {
		t.Run(filepath.Base(img), func(t *testing.T) {
			roundTrip(t, bin, scratch, img)
		})
	}
}

func roundTrip(t *testing.T, bin, scratch, img string) {
	ctx := context.Background()
	src, err := evidence.Open(ctx, img, evidence.Options{Decode: true})
	if err != nil {
		if strings.Contains(err.Error(), "parent image not found") {
			// The chain's ancestor is missing: refused, not read as zeros.
			r := testutil.RunDues(t, bin, scratch, time.Hour, "store", img, "--dbpath", filepath.Join(t.TempDir(), "db"), "--no-index")
			if r.ExitCode == 0 || !strings.Contains(r.Combined(), "parent image not found") {
				t.Fatalf("store of a disk missing its parent: exit %d\n%s", r.ExitCode, r.LastLine())
			}
			t.Logf("refused: %v", err)
			return
		}
		t.Fatalf("open: %v", err)
	}
	info := src.Info()
	src.Close()
	var files int64
	for _, m := range info.Members {
		files += m.Size
	}
	free, err := diskspace.Free(scratch)
	if err != nil {
		t.Fatal(err)
	}
	if need := uint64(files) + uint64(files)/4 + diskspace.Reserve; need > free {
		t.Skipf("its files (%d MiB) would not fit the %d MiB free on the scratch volume", files>>20, free>>20)
	}

	root := filepath.Join(scratch, "db-"+strings.ReplaceAll(filepath.Base(img), " ", "_"))
	_ = os.RemoveAll(root)
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	start := time.Now()
	r := testutil.RunDues(t, bin, scratch, 6*time.Hour, "store", img, "--dbpath", root, "--no-index")
	if r.ExitCode != 0 {
		t.Fatalf("store: exit %d\n%s", r.ExitCode, r.LastLine())
	}
	stored := time.Since(start)
	ev := listEvidence(t, bin, scratch, root)
	if len(ev) != 1 || ev[0].Status != "completed" {
		t.Fatalf("listed %+v", ev)
	}

	h, err := dbopen.Open(ctx, dbopen.Request{Path: root, Mode: dbopen.ReadOnly})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	sum := sha256.New()
	start = time.Now()
	rep, err := store.Restore(ctx, h.DB, ev[0].Hash, sum, store.RestoreOptions{Verify: store.VerifyFull, VerifyEWF: info.EWF != nil})
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	restored := time.Since(start)
	if rep.Digest != store.DigestVerified {
		t.Fatalf("restore: digest %s", rep.Digest)
	}
	got := hex.EncodeToString(sum.Sum(nil))

	switch {
	case info.EWF != nil:
		if rep.EWF != structs.EWFVerified {
			t.Fatalf("restored image against the set's stored digests: %q", rep.EWF)
		}
		t.Logf("E01 verified against its stored digests")
	case info.Format == structs.SourceRaw:
		want := fileSHA256(t, img)
		if got != want {
			t.Fatalf("restored %s, the file is %s", got, want)
		}
	default:
		t.Logf("%s: the decoded disk restores to the hash its ingest recorded", info.Format)
	}
	t.Logf("%d MiB stored in %s, restored and verified in %s (sha256 %s)", rep.Size>>20, stored.Round(time.Second), restored.Round(time.Second), got[:16])
}

func fileSHA256(t *testing.T, path string) string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(h.Sum(nil))
}

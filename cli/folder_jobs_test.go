package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"indicer/internal/testutil"
	"indicer/internal/testutil/testdb"
	"indicer/lib/evidence"
	"indicer/lib/ingest"
	"indicer/lib/logging"
	"indicer/lib/util"
)

// folderForJobs writes a folder that tests what a parallel store must keep:
// distinct files, three copies of one content (which must alias the first
// in folder order), and files of one size with different content (which
// must not).
func folderForJobs(t *testing.T) string {
	t.Helper()
	in := filepath.Join(t.TempDir(), "in")
	for i := range 6 {
		testutil.WriteRandomFile(t, in, "distinct-"+string(rune('a'+i))+".bin", uint64(100+i), 200_000+i*4099)
	}
	// Next to each other in folder order, so four at a time start together.
	same := testutil.RandomBytes(7, 4<<20)
	for _, name := range []string{"copy-1.bin", "copy-2.bin", "copy-3.bin"} {
		p := filepath.Join(in, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, same, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for i := range 3 {
		testutil.WriteRandomFile(t, in, "size-twin-"+string(rune('a'+i))+".bin", uint64(200+i), 150_000)
	}
	return in
}

var (
	evidenceID = regexp.MustCompile(`evidence [A-Za-z0-9+/=]+`)
	tookTime   = regexp.MustCompile(` in [0-9.]+[µnm]?s\b| in [0-9hms.]+$`)
)

// storeReport stores dir with jobs at once into a database of its own and
// returns what it printed, without evidence IDs and times, and which
// evidence each alias line named, by the stored path it belongs to.
func storeReport(t *testing.T, dir string, jobs int) (string, map[string]string) {
	t.Helper()
	d := testdb.Open(t, testdb.Default)
	if err := util.EnsureBlobPath(d.Root); err != nil {
		t.Fatal(err)
	}
	sess := ingest.NewSession(d.DB)
	defer sess.Close()
	var out bytes.Buffer
	if err := storeFolder(t.Context(), &out, sess, dir, StoreOptions{Decode: true, Quiet: true, Jobs: jobs}); err != nil {
		t.Fatalf("jobs %d: %v", jobs, err)
	}
	// Which stored path each alias points at, so runs compare without IDs.
	owner := map[string]string{}
	aliasOf := map[string]string{}
	var current string
	for _, line := range strings.Split(out.String(), "\n") {
		switch {
		case strings.HasPrefix(line, "Storing "):
			current = strings.TrimPrefix(line, "Storing ")
		case strings.HasPrefix(line, "Stored as evidence "):
			owner[evidenceID.FindString(line)] = current
		case strings.HasPrefix(line, "Already stored as evidence "):
			aliasOf[current] = owner[evidenceID.FindString(line)]
		}
	}
	var lines []string
	for _, line := range strings.Split(out.String(), "\n") {
		line = evidenceID.ReplaceAllString(line, "evidence ID")
		lines = append(lines, tookTime.ReplaceAllString(line, " in T"))
	}
	return strings.Join(lines, "\n"), aliasOf
}

// TestFolderJobsReadAsOneAtATime: a folder stored four evidence at a time
// prints what a store of one at a time prints, in the same order, and its
// copies alias the same first copy.
func TestFolderJobsReadAsOneAtATime(t *testing.T) {
	testdb.Isolate(t)
	testdb.EnsureCodecs(t)
	dir := folderForJobs(t)
	one, aliasesOne := storeReport(t, dir, 1)
	four, aliasesFour := storeReport(t, dir, 4)
	if one != four {
		t.Fatalf("jobs 4 printed\n%s\n\njobs 1 printed\n%s", four, one)
	}
	first := filepath.Join(dir, "copy-1.bin")
	if len(aliasesOne) != 2 {
		t.Fatalf("jobs 1 recorded aliases %v, want the two later copies", aliasesOne)
	}
	for path, of := range aliasesFour {
		if of != first || aliasesOne[path] != first {
			t.Fatalf("%s aliases %q with jobs 4 and %q with jobs 1, want %s", path, of, aliasesOne[path], first)
		}
	}
	if len(aliasesFour) != 2 {
		t.Fatalf("jobs 4 recorded aliases %v, want the two later copies", aliasesFour)
	}
}

// TestFolderJobsAliasAcrossFormats: one disk in different formats is one
// content. An EWF set and the raw disk it holds, and a raw disk and its VHD,
// dynamic VHD and VHDX, stored four at a time, alias the first of each in
// folder order, as a store of one at a time does; a disk of the same size
// with other content is stored.
func TestFolderJobsAliasAcrossFormats(t *testing.T) {
	testdb.Isolate(t)
	testdb.EnsureCodecs(t)
	in := filepath.Join(t.TempDir(), "in")
	if err := os.MkdirAll(in, 0o755); err != nil {
		t.Fatal(err)
	}
	place := func(name string, r io.Reader) string {
		t.Helper()
		p := filepath.Join(in, name)
		f, err := os.Create(p)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.Copy(f, r); err != nil {
			t.Fatal(err)
		}
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
		return p
	}
	copyOf := func(name, from string) string {
		t.Helper()
		f, err := os.Open(from)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		return place(name, f)
	}

	m, first := fixtureNamed(t, "ewf/fat12_multiseg")
	var ewf string
	for i, seg := range m.Segments {
		p := copyOf("a-disk"+filepath.Ext(seg), filepath.Join(filepath.Dir(first), seg))
		if i == 0 {
			ewf = p
		}
	}
	src, err := evidence.Open(t.Context(), ewf, evidence.Options{Decode: true})
	if err != nil {
		t.Fatal(err)
	}
	raw := place("b-disk.img", io.NewSectionReader(src, 0, src.Size()))
	src.Close()
	fixture := func(name, as string) string {
		_, p := fixtureNamed(t, name)
		return copyOf(as, p)
	}
	vhd := fixture("vhd/fat16_fixed_vhd", "c-fat16.vhd")
	want := map[string]string{raw: ewf}
	for _, f := range []struct{ name, as string }{
		{"fat/fat16", "d-fat16.img"},
		{"vhd/fat16_dynamic_vhd", "e-fat16-dyn.vhd"},
		{"vhd/fat16_vhdx", "f-fat16.vhdx"},
	} {
		want[fixture(f.name, f.as)] = vhd
	}
	fixture("ext/ext4", "g-ext4.img")

	one, aliasesOne := storeReport(t, in, 1)
	four, aliasesFour := storeReport(t, in, 4)
	if one != four {
		t.Fatalf("jobs 4 printed\n%s\n\njobs 1 printed\n%s", four, one)
	}
	if !maps.Equal(aliasesOne, want) || !maps.Equal(aliasesFour, want) {
		t.Fatalf("aliases with jobs 1 %v and with jobs 4 %v, want %v", aliasesOne, aliasesFour, want)
	}
}

// TestFolderJobsReportFailuresInFolderOrder: evidence stored at once that
// fail are listed in folder order, and the error names the first in folder
// order, whichever failed first.
func TestFolderJobsReportFailuresInFolderOrder(t *testing.T) {
	testdb.Isolate(t)
	testdb.EnsureCodecs(t)
	in := filepath.Join(t.TempDir(), "in")
	for i, name := range []string{"a.bin", "b-bad.bin", "c.bin", "d-bad.bin"} {
		testutil.WriteRandomFile(t, in, name, uint64(300+i), 5000+i*97)
	}
	prev := storeOneFile
	t.Cleanup(func() { storeOneFile = prev })
	laterFailed := make(chan struct{})
	storeOneFile = func(ctx context.Context, w io.Writer, sess *ingest.Session, path string, sopts StoreOptions) (ingest.Bytes, error) {
		switch filepath.Base(path) {
		case "b-bad.bin":
			select { // fail after d-bad.bin; one at a time, d never comes
			case <-laterFailed:
			case <-time.After(10 * time.Second):
			}
			return ingest.Bytes{}, errors.New("b failed")
		case "d-bad.bin":
			close(laterFailed)
			return ingest.Bytes{}, errors.New("d failed")
		}
		return prev(ctx, w, sess, path, sopts)
	}

	d := testdb.Open(t, testdb.Default)
	if err := util.EnsureBlobPath(d.Root); err != nil {
		t.Fatal(err)
	}
	sess := ingest.NewSession(d.DB)
	defer sess.Close()
	var out bytes.Buffer
	err := storeFolder(t.Context(), &out, sess, in, StoreOptions{Decode: true, Quiet: true, Jobs: 4})
	if want := "first " + filepath.Join(in, "b-bad.bin") + ": b failed"; err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("folder store = %v, want an error naming %q", err, want)
	}
	b, dd := strings.Index(out.String(), "failed: "+filepath.Join(in, "b-bad.bin")), strings.Index(out.String(), "failed: "+filepath.Join(in, "d-bad.bin"))
	if b < 0 || dd < b {
		t.Fatalf("failures listed out of folder order:\n%s", out.String())
	}
}

// TestStoreJobs: --store-jobs takes 0 to 64; 0 picks a number, and
// low-resource mode stores one at a time.
func TestStoreJobs(t *testing.T) {
	for _, n := range []int{-1, 65} {
		if _, err := StoreJobs(n); err == nil {
			t.Errorf("StoreJobs(%d) accepted", n)
		}
	}
	if n, err := StoreJobs(0); err != nil || n < 1 || n > 4 {
		t.Errorf("StoreJobs(0) = %d, %v; want 1 to 4", n, err)
	}
	if n, _ := StoreJobs(8); n != 8 {
		t.Errorf("StoreJobs(8) = %d", n)
	}
}

// logRecordsOf logs the rest of the test to a file of its own, and returns
// a reader of its records with a message.
func logRecordsOf(t *testing.T) func(msg string) []map[string]any {
	t.Helper()
	return logRecordsAt(t, false)
}

// logRecordsAt is logRecordsOf, with debug records too when debug is set.
func logRecordsAt(t *testing.T, debug bool) func(msg string) []map[string]any {
	t.Helper()
	info := logging.Init(logging.Config{Dir: t.TempDir(), Stderr: io.Discard, Debug: debug})
	t.Cleanup(func() { _ = logging.Close() })
	return func(msg string) []map[string]any {
		t.Helper()
		_ = logging.Sync()
		b, err := os.ReadFile(info.Path)
		if err != nil {
			t.Fatal(err)
		}
		var recs []map[string]any
		for line := range strings.SplitSeq(string(b), "\n") {
			var r map[string]any
			if json.Unmarshal([]byte(line), &r) == nil && r["msg"] == msg {
				recs = append(recs, r)
			}
		}
		return recs
	}
}

// checkPoolRecord checks what a folder store's or a reindex's completion
// record says of its run: its items and, with jobs above 1, how each worker
// spent it.
func checkPoolRecord(t *testing.T, r map[string]any, items, jobs int) {
	t.Helper()
	if r["items"] != float64(items) || r["jobs"] != float64(jobs) {
		t.Fatalf("%s: %v items at %v jobs, want %d at %d", r["msg"], r["items"], r["jobs"], items, jobs)
	}
	if perSecond, _ := r["items_per_s"].(float64); perSecond <= 0 {
		t.Fatalf("%s: %v items a second", r["msg"], r["items_per_s"])
	}
	busy, _ := r["busy_ms"].([]any)
	held, _ := r["held_ms"].([]any)
	u, _ := r["utilization"].(float64)
	switch {
	case jobs == 1 && (r["busy_ms"] != nil || r["held_ms"] != nil || r["utilization"] != nil):
		t.Fatalf("%s: pool fields for a run of one at a time: %v", r["msg"], r)
	case jobs > 1 && (len(busy) != jobs || len(held) != jobs || u <= 0 || u > 1):
		t.Fatalf("%s: busy_ms %v, held_ms %v, utilization %v; want one per worker and a share", r["msg"], busy, held, u)
	}
}

// TestFolderRecordSaysHowThePoolRan: the StoreFolder COMPLETE record counts
// the items and, when several ran at once, how each worker spent the store.
func TestFolderRecordSaysHowThePoolRan(t *testing.T) {
	testdb.Isolate(t)
	testdb.EnsureCodecs(t)
	records := logRecordsOf(t)
	dir := folderForJobs(t)
	storeReport(t, dir, 1)
	storeReport(t, dir, 4)
	recs := records("StoreFolder COMPLETE")
	if len(recs) != 2 {
		t.Fatalf("%d StoreFolder COMPLETE records, want 2", len(recs))
	}
	checkPoolRecord(t, recs[0], 12, 1)
	checkPoolRecord(t, recs[1], 12, 4)
}

// TestHeavyItemTakesNoCreditLock: a heavy evidence of a parallel folder
// store starts once every evidence before it is reported and settled, so its
// chunk pass never takes the credit ledger's lock; the StoreFolder ITEM
// debug record says so, and says what badger's tree and the runtime did.
func TestHeavyItemTakesNoCreditLock(t *testing.T) {
	testdb.Isolate(t)
	testdb.EnsureCodecs(t)
	records := logRecordsAt(t, true)
	in := filepath.Join(t.TempDir(), "in")
	for i := range 4 {
		testutil.WriteRandomFile(t, in, fmt.Sprintf("a-%02d.bin", i), uint64(300+i), 96_000+i*4099)
	}
	testutil.WriteRandomFile(t, in, "m-heavy.bin", 399, 3<<20)
	for i := range 4 {
		testutil.WriteRandomFile(t, in, fmt.Sprintf("z-%02d.bin", i), uint64(400+i), 96_000+i*4099)
	}
	d := testdb.Open(t, testdb.Default)
	if err := util.EnsureBlobPath(d.Root); err != nil {
		t.Fatal(err)
	}
	sess := ingest.NewSession(d.DB)
	defer sess.Close()
	opts := StoreOptions{Decode: true, Quiet: true, NoIndex: true, Jobs: 4, heavy: 1 << 20}
	if err := storeFolder(t.Context(), io.Discard, sess, in, opts); err != nil {
		t.Fatal(err)
	}
	recs := records("StoreFolder ITEM")
	if len(recs) != 9 {
		t.Fatalf("%d StoreFolder ITEM records, want 9", len(recs))
	}
	var heavy int
	for _, r := range recs {
		if _, ok := r["lsm_after_l0_tables"]; !ok {
			t.Fatalf("ITEM record without LSM fields: %v", r)
		}
		if _, ok := r["gc_cycles"]; !ok {
			t.Fatalf("ITEM record without runtime fields: %v", r)
		}
		if r["heavy"] != true {
			continue
		}
		heavy++
		if !strings.HasSuffix(r["file_path"].(string), "m-heavy.bin") {
			t.Fatalf("heavy item is %v, want m-heavy.bin", r["file_path"])
		}
		if r["credit_locked"] != float64(0) {
			t.Fatalf("heavy item took the credit ledger's lock %v times, want 0", r["credit_locked"])
		}
	}
	if heavy != 1 {
		t.Fatalf("%d heavy ITEM records, want 1", heavy)
	}
}

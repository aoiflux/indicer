package cli

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"indicer/internal/testutil"
	"indicer/internal/testutil/testdb"
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
	info := logging.Init(logging.Config{Dir: t.TempDir(), Stderr: io.Discard})
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

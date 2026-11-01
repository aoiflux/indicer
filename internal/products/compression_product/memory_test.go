package compression_product

import (
	"bytes"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"indicer/internal/testutil"
	"indicer/lib/cnst"
)

func TestMain(m *testing.M) {
	os.Exit(testutil.RunChildScenarios(m, map[string]func(args []string) int{
		"archive-memory": childArchiveMemory,
	}))
}

// childArchiveMemory creates and extracts an archive in the directory
// args[0] with GOMAXPROCS set to args[1], as a library host would, then
// reports the live heap after a collection and whether a process encoder is
// left behind.
func childArchiveMemory(args []string) int {
	procs, err := strconv.Atoi(args[1])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	runtime.GOMAXPROCS(procs)
	dir := args[0]
	src := filepath.Join(dir, "src")
	rng := rand.New(rand.NewPCG(1, 2))
	words := []string{"evidence ", "chunk ", "volume ", "record ", "partition ", "hash "}
	for i := range 8 {
		var b bytes.Buffer
		for b.Len() < 512<<10 {
			b.WriteString(words[rng.IntN(len(words))])
		}
		p := filepath.Join(src, fmt.Sprintf("d%d", i%2), fmt.Sprintf("f%d.txt", i))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		if err := os.WriteFile(p, b.Bytes(), 0o644); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
	}
	svc := New()
	archive := filepath.Join(dir, "a.duesarc")
	if _, rerr := handle(svc, opCreateArchive, map[string]any{"input_path": src, "output_path": archive, "chunk_size_kb": 64}); rerr != nil {
		fmt.Fprintln(os.Stderr, "create:", rerr, rerr.Details)
		return 1
	}
	if _, rerr := handle(svc, opExtractArchive, map[string]any{"input_path": archive, "output_path": filepath.Join(dir, "out")}); rerr != nil {
		fmt.Fprintln(os.Stderr, "extract:", rerr, rerr.Details)
		return 1
	}
	runtime.GC()
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	fmt.Printf("live=%d encoder=%t\n", m.HeapAlloc, cnst.ENCODER != nil)
	return 0
}

// TestArchiveOperationsReleaseEncoders is the FFI-4 regression: a library
// host keeps the process for as long as it runs, so an archive operation must
// not leave encoder state behind. Before, the operation's process encoder
// (best level) and badger's table encoder (level 15) each kept one 50 MB
// state per CPU after the first archive: about 1.1 GB live here.
func TestArchiveOperationsReleaseEncoders(t *testing.T) {
	c := testutil.StartChild(t, "archive-memory", t.TempDir(), "16")
	if code := c.Wait(t, 3*time.Minute); code != 0 {
		t.Fatalf("child exited %d:\n%s", code, c.Output())
	}
	out := c.Output()
	i := strings.LastIndex(out, "live=")
	if i < 0 {
		t.Fatalf("no report from the child:\n%s", out)
	}
	var liveText, encoder string
	if _, err := fmt.Sscanf(out[i:], "live=%s encoder=%s", &liveText, &encoder); err != nil {
		t.Fatalf("report %q: %v", out[i:], err)
	}
	live, err := strconv.ParseUint(liveText, 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	if encoder != "false" {
		t.Errorf("the operation left a process encoder behind")
	}
	const limit = 128 << 20
	if live > limit {
		t.Errorf("live heap after the archive round trip is %d MB, want at most %d MB", live>>20, limit>>20)
	}
}

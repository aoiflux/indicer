package osdirs

import (
	"bufio"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// envReads are the calls that read the process environment, directly or
// through a standard-library or dependency helper. DUES takes every
// setting from a flag and every default directory from this package.
//
// Not listed, accepted: the runtime's GODEBUG, GOMAXPROCS, GOTRACEBACK and
// TZ; os.Getwd and filepath.Abs, which use PWD only after checking it is
// the working directory; and testing's TempDir, which is go test's own.
var envReads = []struct {
	re *regexp.Regexp
	// allow names the files where the call is known not to read the
	// environment.
	allow []string
}{
	{re: regexp.MustCompile(`\bos\.(Getenv|LookupEnv|Environ|ExpandEnv|Setenv|Unsetenv|Clearenv)\b`)},
	// TMPDIR, TMP, TEMP, HOME, USERPROFILE, XDG_*, LOCALAPPDATA, APPDATA.
	{re: regexp.MustCompile(`\bos\.(TempDir|UserHomeDir|UserCacheDir|UserConfigDir)\b`)},
	// An empty directory means os.TempDir.
	{re: regexp.MustCompile(`\bos\.(MkdirTemp|CreateTemp)\(\s*""`)},
	{re: regexp.MustCompile(`\b(syscall|unix|windows)\.(Getenv|Environ|Setenv|Unsetenv|Clearenv|GetTempPath2?|ExpandEnvironmentStrings|GetEnvironmentVariable|KnownFolderPath|SHGetKnownFolderPath)\b`),
		// The test compares the profile folders with the known-folder API.
		allow: []string{"internal/osdirs/osdirs_windows_test.go"}},
	// On Unix it falls back to $USER and $HOME when the account lookup
	// fails. On Windows it reads the process token.
	{re: regexp.MustCompile(`\buser\.Current\b`),
		allow: []string{"internal/osdirs/osdirs_windows.go", "internal/osdirs/osdirs_windows_test.go"}},
	// exec.Cmd.Environ, and os.Environ by another route.
	{re: regexp.MustCompile(`\.Environ\(`)},
	// kingpin flag defaults from the environment.
	{re: regexp.MustCompile(`\.(Envar|DefaultEnvars)\(`)},
	{re: regexp.MustCompile(`\bcolorprofile\.(Env|Detect)\b`)},
}

// envAllowlist holds the environment reads that remain, as
// "path: code" with the comment cut, one entry per occurrence. The
// flags-only pass removes them; the guard fails on a read not listed and
// on an entry that no longer matches, so the list only shrinks.
var envAllowlist = []string{
	"internal/crash/crash_test.go: if _, err := Setup(os.Getenv(\"CRASH_DIR\"), \"child\"); err != nil {",
	"internal/e2e/corpus_test.go: list := os.Getenv(\"DUES_TEST_CORPUS_LIST\")",
	"internal/e2e/pty_test.go: if os.Getenv(\"DUES_REQUIRE_PTY\") == \"1\" {",
	"internal/faultinject/on.go: spec := os.Getenv(\"DUES_FAULTS\")",
	"internal/faultinject/on.go: notify := os.Getenv(\"DUES_FAULTS_NOTIFY\")",
	"internal/faultinject/on_test.go: if runtime.GOOS == \"windows\" && os.Getenv(\"DUES_TEST_CONSOLE\") == \"\" {",
	"internal/products/compression_product/memory_test.go: dir := os.Getenv(envChildDir)",
	"internal/testutil/child.go: name := os.Getenv(EnvChildScenario)",
	"internal/testutil/child.go: cmd.Env = append(os.Environ(), EnvChildScenario+\"=\"+scenario, EnvFaultsNotify+\"=\"+notify)",
	"internal/testutil/child.go: cmd.Env = append(os.Environ(), EnvFaultsNotify+\"=\"+notify)",
	"internal/testutil/dues.go: dir, err := os.MkdirTemp(\"\", \"dues-bin-\")",
	"internal/testutil/dues.go: for _, kv := range os.Environ() {",
	"internal/testutil/env.go: dir := os.Getenv(name)",
	"internal/testutil/env.go: if os.Getenv(\"DUES_LONG_TESTS\") != \"1\" {",
	"internal/testutil/files.go: if os.Getenv(\"DUES_UPDATE_GOLDEN\") == \"1\" {",
	"internal/testutil/fsfixture/fsfixture.go: d, err := os.MkdirTemp(\"\", \"dues-parser-fixtures-\")",
	"internal/testutil/testutil_test.go: f, err := os.OpenFile(os.Getenv(EnvFaultsNotify), os.O_APPEND|os.O_WRONLY, 0)",
	"lib/dbopen/keyrotation_test.go: h, err := Open(context.Background(), Request{Path: os.Getenv(envChildRoot), Mode: ReadOnly, Creds: dbmeta.Password(rotationPassword)})",
	"lib/fsx/tempfile_test.go: f, err := CreateTempDeleteOnClose(os.Getenv(\"DUES_TEST_DIR\"), \"spill-*\")",
	"lib/fsx/tempfile_test.go: n, err := os.OpenFile(os.Getenv(testutil.EnvFaultsNotify), os.O_APPEND|os.O_WRONLY, 0)",
	"lib/parser/fs/ntfs/compressed_test.go: for _, path := range filepath.SplitList(os.Getenv(\"DUES_CORPUS_NTFS\")) {",
}

// guardFile is this file, whose allowlist spells the reads out.
const guardFile = "internal/osdirs/noenv_guard_test.go"

// TestNoEnvironmentReads fails on any new environment read in DUES code,
// tests and test helpers included.
func TestNoEnvironmentReads(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	pending := map[string]int{}
	for _, e := range envAllowlist {
		pending[e]++
	}
	var found []string
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path != root && (strings.HasPrefix(d.Name(), ".") || d.Name() == "testdata" || d.Name() == "vendor") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if rel == guardFile {
			return nil
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		for line := 1; sc.Scan(); line++ {
			code, _, _ := strings.Cut(sc.Text(), "//")
			code = strings.TrimSpace(code)
			for _, r := range envReads {
				if !r.re.MatchString(code) || slices.Contains(r.allow, rel) {
					continue
				}
				entry := rel + ": " + code
				if pending[entry] > 0 {
					pending[entry]--
				} else {
					found = append(found, fmt.Sprintf("%s:%d: %s", rel, line, code))
				}
				break
			}
		}
		return sc.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(found) > 0 {
		t.Errorf("%d environment reads; DUES takes settings from flags and default directories from internal/osdirs:\n%s",
			len(found), strings.Join(found, "\n"))
	}
	var stale []string
	for e, n := range pending {
		for range n {
			stale = append(stale, fmt.Sprintf("%q,", e))
		}
	}
	if len(stale) > 0 {
		slices.Sort(stale)
		t.Errorf("%d envAllowlist entries no longer match; delete them:\n%s", len(stale), strings.Join(stale, "\n"))
	}
}

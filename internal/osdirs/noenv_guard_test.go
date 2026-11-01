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

// guardFile is this file, whose patterns spell the reads out.
const guardFile = "internal/osdirs/noenv_guard_test.go"

// TestNoEnvironmentReads fails on any new environment read in DUES code,
// tests and test helpers included.
func TestNoEnvironmentReads(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
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
				if r.re.MatchString(code) && !slices.Contains(r.allow, rel) {
					found = append(found, fmt.Sprintf("%s:%d: %s", rel, line, code))
					break
				}
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
}

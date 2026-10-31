package ui

import (
	"bufio"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// banned are the ways colour once bypassed DUES's settings:
//   - fatih/color decided from stdout alone and coloured stderr regardless;
//   - lipgloss/v2/compat queries the terminal from its package init (slow
//     over SSH, stray bytes in the shell);
//   - lipgloss.Print* and Fprint* re-detect from the environment and ignore
//     --color and --plain.
var banned = regexp.MustCompile(`"github\.com/fatih/color"|"charm\.land/lipgloss/v2/compat"|\blipgloss\.(Print|Println|Printf|Fprint|Fprintln|Fprintf)\b`)

func TestNoColourBypasses(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	var offenders []string
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if strings.HasPrefix(d.Name(), ".") || d.Name() == "testdata" || d.Name() == "vendor" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "guard_test.go") {
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
			if banned.MatchString(code) {
				rel, _ := filepath.Rel(root, path)
				offenders = append(offenders, filepath.ToSlash(rel)+":"+strconv.Itoa(line)+": "+strings.TrimSpace(sc.Text()))
			}
		}
		return sc.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(offenders) > 0 {
		t.Fatalf("colour that bypasses internal/ui:\n%s", strings.Join(offenders, "\n"))
	}
}

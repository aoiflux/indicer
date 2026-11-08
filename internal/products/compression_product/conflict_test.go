package compression_product

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

// An extraction that will be refused for a conflict writes no more content:
// an archive extracted with "error" over an existing tree, or with a file
// where a directory is, is not first restored into the staging directory.
func TestExtractConflictsFoundBeforeWriting(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		policy   string
		existing map[string]bool // name -> is a directory
		// conflicts counts the conflicting entries; written lists the
		// entries whose content was written, in order.
		conflicts int
		written   []string
	}{
		{conflictError, map[string]bool{"a.txt": false, "c.txt": false}, 2, nil},
		{conflictOverwrite, map[string]bool{"a.txt": true}, 1, nil},
		{conflictSkip, map[string]bool{"b.txt": true}, 1, []string{"a.txt"}},
	} {
		t.Run(c.policy, func(t *testing.T) {
			out := t.TempDir()
			for name, dir := range c.existing {
				var err error
				if dir {
					err = os.Mkdir(filepath.Join(out, name), 0o755)
				} else {
					err = os.WriteFile(filepath.Join(out, name), []byte("old"), 0o644)
				}
				if err != nil {
					t.Fatal(err)
				}
			}
			before := listDir(t, out)
			limits, lerr := Limits{}.resolve()
			if lerr != nil {
				t.Fatal(lerr)
			}
			x, err := newExtraction(out, limits, c.policy, nil)
			if err != nil {
				t.Fatal(err)
			}
			var written []string
			for _, name := range []string{"a.txt", "b.txt", "c.txt"} {
				err := x.addFile(name, int64(len(name)), time.Time{}, func(w io.Writer) error {
					written = append(written, name)
					_, err := io.WriteString(w, name)
					return err
				})
				if err != nil {
					t.Fatal(err)
				}
			}
			_, err = x.commit()
			var ce *ConflictError
			if !errors.As(err, &ce) || ce.Count != c.conflicts {
				t.Fatalf("commit: %v, want %d conflicts", err, c.conflicts)
			}
			if !slices.Equal(written, c.written) {
				t.Fatalf("content written for %v, want %v", written, c.written)
			}
			if after := listDir(t, out); !slices.Equal(after, before) {
				t.Fatalf("output %v, want it unchanged (%v)", after, before)
			}
		})
	}
}

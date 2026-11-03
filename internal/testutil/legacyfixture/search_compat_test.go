package legacyfixture

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"indicer/internal/testutil"
)

func exitCode(err error) int {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	if err != nil {
		return -1
	}
	return 0
}

// TestLegacySearchAndNear: search and near run on databases written before
// this pass. Search writes its report; damage the writing release left
// (STO-1) makes it exit 2, naming what it could not read. Near on evidence
// stored without reverse relations says to add them; once repair --fix
// --revrel has, near runs.
func TestLegacySearchAndNear(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the dues binary")
	}
	bin := testutil.BuildDues(t)
	for _, f := range Load(t) {
		t.Run(f.Mode, func(t *testing.T) {
			db := Extract(t, f)
			work := t.TempDir()
			report := filepath.Join(work, "search.json")
			out, err := runIn(t, work, bin, append([]string{"search", "the", "--out", report, "--dbpath", db}, f.ReadFlags(t)...)...)
			if code := exitCode(err); code != 0 && code != 2 {
				t.Fatalf("search exit %d: %s", code, lastLine(out))
			}
			body, err := os.ReadFile(report)
			if err != nil {
				t.Fatalf("search wrote no report: %v; %s", err, lastLine(out))
			}
			var r struct {
				Coverage struct {
					EvidenceSearched int64 `json:"evidence_searched"`
				} `json:"coverage"`
			}
			if err := json.Unmarshal(body, &r); err != nil || r.Coverage.EvidenceSearched == 0 {
				t.Fatalf("search report: %v, %s", err, body)
			}

			var hash string
			for _, rs := range f.Restores {
				if rs.Kind == "evidence" && rs.Status == "ok" {
					hash = rs.Hash
					break
				}
			}
			if hash == "" {
				t.Skip("no evidence restored correctly when generated")
			}
			nearArgs := append([]string{"near", "in", hash, "--out", filepath.Join(work, "near.json"), "--force", "--dbpath", db}, f.ReadFlags(t)...)
			out, err = runIn(t, work, bin, nearArgs...)
			if code := exitCode(err); code != 0 && code != 2 {
				if !strings.Contains(out, "repair --fix --revrel") {
					t.Fatalf("near exit %d without naming the repair: %s", code, lastLine(out))
				}
				rout, rerr := runIn(t, work, bin, append([]string{"repair", "--fix", "--revrel", "--dbpath", db}, f.ReadFlags(t)...)...)
				if rerr != nil {
					t.Fatalf("repair --fix --revrel: %v: %s", rerr, lastLine(rout))
				}
				out, err = runIn(t, work, bin, nearArgs...)
				if code := exitCode(err); code != 0 && code != 2 {
					t.Fatalf("near after repair --fix --revrel: exit %d: %s", code, lastLine(out))
				}
			}
		})
	}
}

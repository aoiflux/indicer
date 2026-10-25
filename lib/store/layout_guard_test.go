package store

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// layoutGuarded lists the storage files written or rewritten from W3 on.
// They must take the database layout from the database, through dbmeta or
// the evidence record, never from the process-wide layout globals, which a
// second database open in the same process (the server, the FFI) would
// change underneath them (I3). Add each rewritten file here.
var layoutGuarded = []string{
	"lib/dbio/blobpath.go",
	"lib/dbio/chunk_stored.go",
	"lib/dbio/extents.go",
	"lib/dbio/locator.go",
	"lib/dbio/logical.go",
	"lib/dbio/members.go",
	"lib/dbio/objects.go",
	"lib/dbio/recordref.go",
	"lib/dbio/revrel_scan.go",
	"lib/evidence/evidence.go",
	"lib/evidence/plan.go",
	"lib/index/hash.go",
	"lib/index/ids.go",
	"lib/index/index.go",
	"lib/index/materialize.go",
	"lib/index/partition.go",
	"lib/index/record.go",
	"lib/fio/blockindex.go",
	"lib/fio/container.go",
	"lib/fio/container_cache.go",
	"lib/fio/fio.go",
	"lib/fsx/fsx.go",
	"lib/fsx/tempfile.go",
	"lib/store/batch_owner.go",
	"lib/store/chunkfeed.go",
	"lib/store/chunkpass.go",
	"lib/store/commit.go",
	"lib/store/durability.go",
	"lib/store/hashpass.go",
	"lib/store/index_delete.go",
	"lib/store/staged_pipeline.go",
	"lib/server/rpc.go",
	"lib/server/rpc_files.go",
	"lib/server/upload.go",
	"cli/cmdtui.go",
	"lib/store/ingest_validation.go",
	"lib/store/restore.go",
	"lib/store/stats.go",
	"lib/store/upgrade.go",
	"lib/store/verify.go",
}

var layoutGlobal = regexp.MustCompile(`\bcnst\.(ChonkSize|QUICKOPT|HASHALGO)\b`)

// TestRewrittenFilesAvoidLayoutGlobals fails when a guarded file reads a
// layout global.
func TestRewrittenFilesAvoidLayoutGlobals(t *testing.T) {
	root := filepath.Join("..", "..")
	for _, rel := range layoutGuarded {
		src, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatalf("%s: %v (remove it from layoutGuarded if it moved)", rel, err)
		}
		for i, line := range strings.Split(string(src), "\n") {
			code, _, _ := strings.Cut(line, "//")
			if m := layoutGlobal.FindString(code); m != "" {
				t.Errorf("%s:%d reads %s; take the layout from the database (dbmeta.LayoutFor)", rel, i+1, m)
			}
		}
	}
}

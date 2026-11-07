package store

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// layoutGuarded lists the storage files written or rewritten from W3 on.
// They must take the database layout from the database, through dbmeta,
// engine.For or the evidence record, and its tuning and encoder from
// engine.For, never from the process-wide globals, which a second database
// open in the same process (the server, the FFI) would change underneath
// them (I3). The code that turns flags into settings and opens databases
// is listed too: it passes a cnst.Tuning and sets nothing process-wide.
// Add each rewritten file here.
var layoutGuarded = []string{
	"config_apply.go",
	"main.go",
	"cli/cmdpackage.go",
	"cli/common.go",
	"internal/products/compression_product/engine.go",
	"lib/dbopen/storage.go",
	"lib/dbpackage/deep.go",
	"lib/dbpackage/export.go",
	"lib/dbpackage/import.go",
	"lib/fio/write_backend.go",
	"lib/fio/write_backend_iouring_linux.go",
	"lib/dbio/blobpath.go",
	"lib/dbio/chunk_stored.go",
	"lib/dbio/dbio.go",
	"lib/dbio/evidence_update.go",
	"lib/dbio/extents.go",
	"lib/dbio/identity.go",
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
	"cli/cmdreindex.go",
	"cli/cmdrepair.go",
	"cli/cmdstore.go",
	"cli/cmdtui.go",
	"cli/folder_jobs.go",
	"lib/ingest/ingest.go",
	"lib/near/near.go",
	"lib/near/report.go",
	"lib/near/verify.go",
	"lib/search/engine.go",
	"lib/store/chunk_writes.go",
	"lib/store/ingest_validation.go",
	"lib/store/restore.go",
	"lib/store/revrel_add.go",
	"lib/store/stats.go",
	"lib/store/store.go",
	"lib/store/upgrade.go",
	"lib/store/verify.go",
}

var layoutGlobal = regexp.MustCompile(`\bcnst\.(` +
	`ChonkSize|QUICKOPT|ENCODER|HASHALGO|CONTAINERMODE|LegacyBlockIndex|GetHashAlgo|` +
	`MEMOPT|CompressLevel|StoreHashStrategy|StorePipelineMode|StoreIOEngine|StoreIOUringQueueDepth|` +
	`StoreWorkerCount|StoreTaskQueueDepth|RestoreWorkerCount|RestoreTaskQueueDepth|RestoreWriteBufferMB|` +
	`Globals|GetMaxThreadCount|GetStorePipelineTuning|GetRestorePipelineTuning|GetRestoreWriteBufferSize|GetStoreIOUringQueueDepth|` +
	`SetHashAlgo|SetStoreHashStrategy|SetStorePipeline|SetStoreIOEngine|SetStoreIOUringQueueDepth|SetHochoMode` +
	`)\b`)

// TestRewrittenFilesAvoidLayoutGlobals fails when a guarded file reads or
// sets a layout, tuning or encoder global.
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
				t.Errorf("%s:%d uses %s; take the layout, tuning and encoder from the database (dbmeta, engine.For), or pass a cnst.Tuning", rel, i+1, m)
			}
		}
	}
}

package server

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"

	"indicer/internal/testutil/fsfixture"
	"indicer/lib/cnst"
	"indicer/lib/dbio"
	"indicer/lib/dbmeta"
	"indicer/lib/ingest"
	"indicer/lib/structs"
	"indicer/lib/util"
	"indicer/pb"
	"indicer/pb/pbconnect"

	"connectrpc.com/connect"
	"github.com/dgraph-io/badger/v4"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

func TestMain(m *testing.M) {
	code := m.Run()
	fsfixture.Cleanup()
	os.Exit(code)
}

func fixture(t *testing.T, name string) string {
	t.Helper()
	for _, m := range fsfixture.All(t) {
		if m.Name == name {
			return fsfixture.Open(t, m)
		}
	}
	t.Fatalf("no fixture %s", name)
	return ""
}

// storeFile ingests path into db the way an upload is stored.
func storeFile(t *testing.T, db *badger.DB, path string) ingest.Result {
	t.Helper()
	res, err := ingest.Run(context.Background(), db, ingest.Request{Path: path, Decode: true, Quiet: true})
	if err != nil {
		t.Fatalf("ingest %s: %v", path, err)
	}
	return res
}

func plainHash(db *badger.DB, data []byte) string {
	h := dbmeta.FileHasher(db)()
	h.Write(data)
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

// rpcServer serves a database holding the given fixtures, and returns a
// Connect client for it.
func rpcServer(t *testing.T, deps Deps, fixtures ...string) (*liveServer, pbconnect.DuesServiceClient, map[string]ingest.Result) {
	t.Helper()
	h := openDB(t)
	if err := util.EnsureBlobPath(h.Root); err != nil {
		t.Fatal(err)
	}
	stored := map[string]ingest.Result{}
	for _, f := range fixtures {
		stored[f] = storeFile(t, h.DB, fixture(t, f))
	}
	deps.DB, deps.Root = h.DB, h.Root
	ls := startServer(t, Config{}, deps, nil)
	return ls, pbconnect.NewDuesServiceClient(http.DefaultClient, ls.url), stored
}

func evidenceID(raw []byte) string {
	return base64.StdEncoding.EncodeToString(util.AppendToBytesSlice(cnst.EviFileNamespace, raw))
}

// SRV-6: the evidence listing is paged, in the order evidence was stored,
// and lists stored evidence once, whatever its aliases.
func TestGetEviFilesPages(t *testing.T) {
	ls, c, stored := rpcServer(t, Deps{}, "fat/fat12", "fat/fat16", "exfat/exfat")
	if res := storeFile(t, ls.deps.DB, fixture(t, "fat/fat16")); string(res.EvidenceID) != string(stored["fat/fat16"].EvidenceID) {
		t.Fatal("storing fat16 again made another evidence")
	}
	var got []string
	token := ""
	for pages := 0; ; pages++ {
		res, err := c.GetEviFiles(context.Background(), connect.NewRequest(&pb.GetEviFilesReq{PageSize: 1, PageToken: token}))
		if err != nil {
			t.Fatal(err)
		}
		if len(res.Msg.EviFile) > 1 || pages > 5 {
			t.Fatalf("page %d holds %d evidence", pages, len(res.Msg.EviFile))
		}
		for _, f := range res.Msg.EviFile {
			got = append(got, f.FileId)
			if len(f.ChunkMap) == 0 || f.ChunkMapTruncated || f.FileSize <= 0 {
				t.Errorf("%s: %d chunks (truncated %v), size %d", f.FilePath, len(f.ChunkMap), f.ChunkMapTruncated, f.FileSize)
			}
		}
		if token = res.Msg.NextPageToken; token == "" {
			break
		}
	}
	var want []string
	for _, r := range stored {
		want = append(want, evidenceID(r.EvidenceID))
	}
	sort.Strings(want)
	sorted := slices.Clone(got)
	sort.Strings(sorted)
	if !slices.Equal(sorted, want) {
		t.Fatalf("listed %v, want %v", got, want)
	}
	// Evidence IDs are UUIDv7 and keys sort by them, so the listing is in
	// the order evidence was stored in.
	var keys []string
	for _, id := range got {
		b, _ := base64.StdEncoding.DecodeString(id)
		keys = append(keys, string(b))
	}
	if !slices.IsSorted(keys) {
		t.Errorf("listed out of key order: %v", got)
	}
}

// SRV-6: an evidence's partitions are listed in the order they lie in it,
// and a partition's files a page at a time, named by their paths. Before,
// a file's evidence was found by parsing its record's name, which records
// written since W6 no longer carry.
func TestGetPartiAndIdxFiles(t *testing.T) {
	ls, c, stored := rpcServer(t, Deps{}, "part/gpt_multi")
	db := ls.deps.DB
	eid := evidenceID(stored["part/gpt_multi"].EvidenceID)
	pres, err := c.GetPartiFiles(context.Background(), connect.NewRequest(&pb.GetPartiFilesReq{EviFileId: eid}))
	if err != nil {
		t.Fatal(err)
	}
	parts := pres.Msg.PartitionFile
	if len(parts) != len(stored["part/gpt_multi"].Partitions) || len(parts) < 2 {
		t.Fatalf("listed %d partitions, the ingest wrote %d", len(parts), len(stored["part/gpt_multi"].Partitions))
	}
	var starts []int64
	for _, p := range parts {
		raw, _ := base64.StdEncoding.DecodeString(p.FileId)
		rec, err := dbio.GetPartitionFile(raw, db)
		if err != nil {
			t.Fatal(err)
		}
		starts = append(starts, rec.Start)
	}
	if !slices.IsSorted(starts) {
		t.Errorf("partitions out of order: starts %v", starts)
	}
	var paged, all []string
	for _, p := range parts {
		all = append(all, p.FileId)
	}
	for token := ""; ; {
		res, err := c.GetPartiFiles(context.Background(), connect.NewRequest(&pb.GetPartiFilesReq{EviFileId: eid, PageSize: 1, PageToken: token}))
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range res.Msg.PartitionFile {
			paged = append(paged, p.FileId)
		}
		if token = res.Msg.NextPageToken; token == "" {
			break
		}
	}
	if !slices.Equal(paged, all) {
		t.Fatalf("paged %v, want %v", paged, all)
	}

	files := 0
	for _, p := range parts {
		var names []string
		for token := ""; ; {
			res, err := c.GetIdxFiles(context.Background(), connect.NewRequest(&pb.GetIdxFilesReq{PartiFileId: p.FileId, PageSize: 2, PageToken: token}))
			if err != nil {
				t.Fatalf("%s: %v", p.FilePath, err)
			}
			if len(res.Msg.IndexedFile) > 2 {
				t.Fatalf("a page of 2 holds %d", len(res.Msg.IndexedFile))
			}
			for _, f := range res.Msg.IndexedFile {
				names = append(names, f.FilePath)
				if strings.Contains(f.FilePath, "|||") || !strings.HasPrefix(f.FilePath, "/") {
					t.Errorf("file named %q", f.FilePath)
				}
				for h := range f.ChunkMap {
					if _, ok := p.ChunkMap[h]; !ok {
						t.Errorf("%s: chunk %s is not one of its partition's", f.FilePath, h)
					}
				}
			}
			if token = res.Msg.NextPageToken; token == "" {
				break
			}
		}
		raw, _ := base64.StdEncoding.DecodeString(p.FileId)
		rec, err := dbio.GetPartitionFile(raw, db)
		if err != nil {
			t.Fatal(err)
		}
		if want := dbio.PartitionMemberCount(rec); len(names) != want {
			t.Errorf("%s: listed %d files, the partition has %d", p.FilePath, len(names), want)
		}
		sorted := slices.Clone(names)
		sort.Strings(sorted)
		if len(slices.Compact(sorted)) != len(names) {
			t.Errorf("%s: a file was listed twice: %v", p.FilePath, names)
		}
		files += len(names)
	}
	if files == 0 {
		t.Fatal("no indexed files listed")
	}
}

// A response carries at most a budget of chunk-map entries: a page ends
// before an object that would not fit, and an object bigger than the whole
// budget comes alone, flagged truncated.
func TestChunkMapBudget(t *testing.T) {
	_, c, _ := rpcServer(t, Deps{}, "fat/fat12", "fat/fat16")
	full, err := c.GetEviFiles(context.Background(), connect.NewRequest(&pb.GetEviFilesReq{}))
	if err != nil {
		t.Fatal(err)
	}
	a, b := len(full.Msg.EviFile[0].ChunkMap), len(full.Msg.EviFile[1].ChunkMap)
	if a < 2 || b < 2 {
		t.Fatalf("evidence of %d and %d chunks; the test needs more", a, b)
	}
	defer func(n int) { chunkEntriesPerResponse = n }(chunkEntriesPerResponse)

	// Room for the first, not both: the second starts the next page.
	chunkEntriesPerResponse = a + b - 1
	p1, err := c.GetEviFiles(context.Background(), connect.NewRequest(&pb.GetEviFilesReq{}))
	if err != nil {
		t.Fatal(err)
	}
	if len(p1.Msg.EviFile) != 1 || p1.Msg.EviFile[0].ChunkMapTruncated || p1.Msg.NextPageToken == "" {
		t.Fatalf("page 1: %d evidence, next %q", len(p1.Msg.EviFile), p1.Msg.NextPageToken)
	}
	p2, err := c.GetEviFiles(context.Background(), connect.NewRequest(&pb.GetEviFilesReq{PageToken: p1.Msg.NextPageToken}))
	if err != nil {
		t.Fatal(err)
	}
	if len(p2.Msg.EviFile) != 1 || p2.Msg.EviFile[0].FileId != full.Msg.EviFile[1].FileId || p2.Msg.NextPageToken != "" {
		t.Fatalf("page 2: %d evidence, next %q", len(p2.Msg.EviFile), p2.Msg.NextPageToken)
	}

	// Less than one object: it comes alone, truncated.
	chunkEntriesPerResponse = 1
	p, err := c.GetEviFiles(context.Background(), connect.NewRequest(&pb.GetEviFilesReq{}))
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Msg.EviFile) != 1 || !p.Msg.EviFile[0].ChunkMapTruncated || len(p.Msg.EviFile[0].ChunkMap) != 1 || p.Msg.NextPageToken == "" {
		t.Fatalf("a 1-entry budget: %d evidence, truncated %v", len(p.Msg.EviFile), p.Msg.EviFile[0].ChunkMapTruncated)
	}
}

func TestListingErrors(t *testing.T) {
	ls, c, stored := rpcServer(t, Deps{}, "fat/fat16")
	ctx := context.Background()
	unknown := evidenceID([]byte("0123456789abcdef"))
	for name, call := range map[string]func() error{
		"negative page": func() error {
			_, err := c.GetEviFiles(ctx, connect.NewRequest(&pb.GetEviFilesReq{PageSize: -1}))
			return err
		},
		"bad token": func() error {
			_, err := c.GetEviFiles(ctx, connect.NewRequest(&pb.GetEviFilesReq{PageToken: "%%%"}))
			return err
		},
		"no evidence id": func() error {
			_, err := c.GetPartiFiles(ctx, connect.NewRequest(&pb.GetPartiFilesReq{}))
			return err
		},
		"bad evidence id": func() error {
			_, err := c.GetPartiFiles(ctx, connect.NewRequest(&pb.GetPartiFilesReq{EviFileId: "not base64!"}))
			return err
		},
		"no partition id": func() error {
			_, err := c.GetIdxFiles(ctx, connect.NewRequest(&pb.GetIdxFilesReq{}))
			return err
		},
	} {
		if got := connect.CodeOf(call()); got != connect.CodeInvalidArgument {
			t.Errorf("%s: %v, want invalid_argument", name, got)
		}
	}
	if _, err := c.GetPartiFiles(ctx, connect.NewRequest(&pb.GetPartiFilesReq{EviFileId: unknown})); connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("unknown evidence: %v", err)
	}
	if _, err := c.GetIdxFiles(ctx, connect.NewRequest(&pb.GetIdxFilesReq{PartiFileId: base64.StdEncoding.EncodeToString([]byte("P|||:nope"))})); connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("unknown partition: %v", err)
	}

	// An index quarantined as wrong lists no partitions and no files.
	ev := stored["fat/fat16"]
	parts, err := c.GetPartiFiles(ctx, connect.NewRequest(&pb.GetPartiFilesReq{EviFileId: evidenceID(ev.EvidenceID)}))
	if err != nil || len(parts.Msg.PartitionFile) == 0 {
		t.Fatalf("partitions before quarantine: %v", err)
	}
	err = dbio.UpdateEvidence(ls.deps.DB, ev.EvidenceID, func(_ *badger.Txn, e *structs.EvidenceFile) error {
		e.Index.State = structs.IndexInvalid
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	after, err := c.GetPartiFiles(ctx, connect.NewRequest(&pb.GetPartiFilesReq{EviFileId: evidenceID(ev.EvidenceID)}))
	if err != nil || len(after.Msg.PartitionFile) != 0 {
		t.Errorf("partitions of a quarantined index: %v, %d", err, len(after.Msg.PartitionFile))
	}
	files, err := c.GetIdxFiles(ctx, connect.NewRequest(&pb.GetIdxFilesReq{PartiFileId: parts.Msg.PartitionFile[0].FileId}))
	if err != nil || len(files.Msg.IndexedFile) != 0 {
		t.Errorf("files of a quarantined index: %v, %d", err, len(files.Msg.IndexedFile))
	}
}

// SRV-5: AppendIfExists finds content by its hash and records the client's
// path as an alias. It used to look the hash up as an evidence key, which
// finds nothing since evidence keys are UUIDs, and to rename the evidence.
func TestAppendIfExistsRecordsAlias(t *testing.T) {
	ls, c, stored := rpcServer(t, Deps{}, "fat/fat16")
	db := ls.deps.DB
	ev := stored["fat/fat16"]
	img, err := os.ReadFile(fixture(t, "fat/fat16"))
	if err != nil {
		t.Fatal(err)
	}
	before, err := dbio.GetEvidenceFile(ev.EvidenceID, db)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for i, want := range []bool{true, false} {
		res, err := c.AppendIfExists(ctx, connect.NewRequest(&pb.AppendIfExistsReq{FileHash: plainHash(db, img), FilePath: `C:\case\copy.img`}))
		if err != nil {
			t.Fatal(err)
		}
		m := res.Msg
		if !m.Exists || m.Appended != want || m.EviFile.GetFileId() != evidenceID(ev.EvidenceID) || len(m.EviFile.GetChunkMap()) == 0 {
			t.Fatalf("call %d: exists %v appended %v file %+v", i, m.Exists, m.Appended, m.EviFile)
		}
	}
	after, err := dbio.GetEvidenceFile(ev.EvidenceID, db)
	if err != nil {
		t.Fatal(err)
	}
	if after.Name != before.Name {
		t.Errorf("the evidence was renamed from %q to %q", before.Name, after.Name)
	}
	aliases, err := dbio.Aliases(db, ev.EvidenceID)
	if err != nil {
		t.Fatal(err)
	}
	if len(aliases) != 1 || aliases[0].Path != `C:\case\copy.img` || aliases[0].Name != "copy.img" || aliases[0].Source.Format != structs.SourceClient {
		t.Fatalf("aliases %+v", aliases)
	}
	// Its content root names it too.
	if res, err := c.AppendIfExists(ctx, connect.NewRequest(&pb.AppendIfExistsReq{FileHash: after.ContentRoot, FilePath: "/x"})); err != nil || !res.Msg.Exists {
		t.Errorf("by content root: %v, %+v", err, res)
	}
	other := plainHash(db, []byte("not stored"))
	if res, err := c.AppendIfExists(ctx, connect.NewRequest(&pb.AppendIfExistsReq{FileHash: other, FilePath: "/x"})); err != nil || res.Msg.Exists {
		t.Errorf("content not stored: %v, %+v", err, res)
	}
	for name, req := range map[string]*pb.AppendIfExistsReq{
		"no hash":   {FilePath: "/x"},
		"bad hash":  {FileHash: "***", FilePath: "/x"},
		"no path":   {FileHash: other},
		"long path": {FileHash: other, FilePath: strings.Repeat("a", maxClientPath+1)},
	} {
		if _, err := c.AppendIfExists(ctx, connect.NewRequest(req)); connect.CodeOf(err) != connect.CodeInvalidArgument {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestSearchRPC(t *testing.T) {
	h := openDB(t)
	if err := util.EnsureBlobPath(h.Root); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "notes.txt")
	if err := os.WriteFile(path, []byte("a Needle, a NEEDLE and a needle in the hay\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ev := storeFile(t, h.DB, path)
	ls := startServer(t, Config{}, Deps{DB: h.DB, Root: h.Root}, nil)
	ctx := context.Background()

	// Over Connect, and over gRPC on cleartext HTTP/2.
	var p http.Protocols
	p.SetUnencryptedHTTP2(true)
	h2 := &http.Client{Transport: &http.Transport{Protocols: &p}}
	for name, c := range map[string]pbconnect.DuesServiceClient{
		"connect": pbconnect.NewDuesServiceClient(http.DefaultClient, ls.url),
		"grpc":    pbconnect.NewDuesServiceClient(h2, ls.url, connect.WithGRPC()),
	} {
		res, err := c.Search(ctx, connect.NewRequest(&pb.SearchReq{Keyword: "  needle "}))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if res.Msg.TotalCount != 3 || res.Msg.KeywordCountMap[evidenceID(ev.EvidenceID)] != 3 || res.Msg.Truncated {
			t.Errorf("%s: total %d, map %v", name, res.Msg.TotalCount, res.Msg.KeywordCountMap)
		}
		for _, q := range []string{"", " ", "x", strings.Repeat("y", 2000)} {
			if _, err := c.Search(ctx, connect.NewRequest(&pb.SearchReq{Keyword: q})); connect.CodeOf(err) != connect.CodeInvalidArgument {
				t.Errorf("%s: keyword of %d bytes: %v", name, len(q), err)
			}
		}
	}
}

// SRV-6: an internal failure reaches the client as "internal error" and a
// request ID; the detail goes to the log only. A panic is contained the same
// way, and the server goes on.
func TestRPCErrorsAreSanitised(t *testing.T) {
	core, logs := observer.New(zap.InfoLevel)
	ls, c, stored := rpcServer(t, Deps{Log: zap.New(core)}, "fat/fat12")
	ctx := context.Background()

	defer func(f func(*badger.DB, []byte, dbio.ReadOptions) (*dbio.Logical, error)) { openLogical = f }(openLogical)
	openLogical = func(*badger.DB, []byte, dbio.ReadOptions) (*dbio.Logical, error) {
		return nil, fmt.Errorf("blob %s is unreadable", filepath.Join(ls.deps.Root, "blob", "secret"))
	}
	_, err := c.GetEviFiles(ctx, connect.NewRequest(&pb.GetEviFilesReq{}))
	if connect.CodeOf(err) != connect.CodeInternal || strings.Contains(err.Error(), "secret") || !strings.Contains(err.Error(), "request ") {
		t.Fatalf("internal failure reached the client as %v", err)
	}
	if logs.FilterMessage("server rpc failed").Len() != 1 {
		t.Errorf("the failure was not logged")
	}

	openLogical = func(*badger.DB, []byte, dbio.ReadOptions) (*dbio.Logical, error) { panic("boom") }
	_, err = c.GetEviFiles(ctx, connect.NewRequest(&pb.GetEviFilesReq{}))
	if connect.CodeOf(err) != connect.CodeInternal || strings.Contains(err.Error(), "boom") || !strings.Contains(err.Error(), "request ") {
		t.Fatalf("a panic reached the client as %v", err)
	}
	if logs.FilterMessage("server PANIC").Len() != 1 {
		t.Errorf("the panic was not logged")
	}
	if resp, _ := do(t, newReq(t, "GET", ls.url+"/healthz", nil)); resp.StatusCode != 200 {
		t.Errorf("after a panic: %d", resp.StatusCode)
	}
	_ = stored
}

// A unary message is bounded after decompression, so a small compressed
// request cannot expand into a large one.
func TestRPCMessageLimit(t *testing.T) {
	ls, c, _ := rpcServer(t, Deps{})
	url := ls.url
	ctx := context.Background()
	big := &pb.AppendIfExistsReq{FileHash: "aGFzaA==", FilePath: strings.Repeat("a", 2<<20)}
	if _, err := c.AppendIfExists(ctx, connect.NewRequest(big)); connect.CodeOf(err) != connect.CodeResourceExhausted {
		t.Errorf("a 2 MiB request: %v", err)
	}
	// Gzipped, the same request is a few kilobytes on the wire.
	gz := pbconnect.NewDuesServiceClient(http.DefaultClient, url, connect.WithSendGzip())
	if _, err := gz.AppendIfExists(ctx, connect.NewRequest(big)); connect.CodeOf(err) != connect.CodeResourceExhausted {
		t.Errorf("a 2 MiB request, gzipped: %v", err)
	}
}

package server

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"indicer/internal/testutil"
	"indicer/lib/cnst"
	"indicer/lib/dbio"
	"indicer/lib/ingest"
	"indicer/lib/structs"
	"indicer/lib/util"
	"indicer/pb"
	"indicer/pb/pbconnect"

	"connectrpc.com/connect"
	"github.com/dgraph-io/badger/v4"
	"github.com/google/uuid"
)

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// uploadServer serves a fresh database; setup may set the upload seams.
func uploadServer(t *testing.T, lim *UploadLimits, setup func(*uploads)) *liveServer {
	t.Helper()
	h := openDB(t)
	if err := util.EnsureBlobPath(h.Root); err != nil {
		t.Fatal(err)
	}
	l := testLimits()
	if lim != nil {
		l.Upload = *lim
	}
	return startServer(t, Config{Limits: l}, Deps{DB: h.DB, Root: h.Root}, func(s *Server) {
		if setup != nil {
			setup(s.uploads)
		}
	})
}

type reply struct {
	status int
	header http.Header
	body   map[string]any
}

func (r reply) str(k string) string { s, _ := r.body[k].(string); return s }

func (r reply) num(k string) int64 { f, _ := r.body[k].(float64); return int64(f) }

func post(t *testing.T, req *http.Request) reply {
	t.Helper()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	out := reply{status: resp.StatusCode, header: resp.Header}
	if len(b) > 0 {
		if err := json.Unmarshal(b, &out.body); err != nil {
			t.Fatalf("%s: %d %q is not JSON", req.URL.Path, resp.StatusCode, b)
		}
	}
	return out
}

func postJSON(t *testing.T, ls *liveServer, path string, v any) reply {
	t.Helper()
	b, _ := json.Marshal(v)
	return post(t, newReq(t, "POST", ls.url+path, bytes.NewReader(b)))
}

// chunk sends data at off; off < 0 sends no Upload-Offset.
func chunk(t *testing.T, ls *liveServer, id string, off int64, data []byte) reply {
	t.Helper()
	req := newReq(t, "POST", ls.url+"/web/upload/chunk", bytes.NewReader(data))
	req.Header.Set("Upload-Id", id)
	if off >= 0 {
		req.Header.Set("Upload-Offset", strconv.FormatInt(off, 10))
	}
	return post(t, req)
}

func start(t *testing.T, ls *liveServer, path, typ, hash string, size int64) reply {
	t.Helper()
	body := map[string]any{"file_path": path, "file_type": typ, "file_hash": hash}
	if size >= 0 {
		body["size"] = size
	}
	return postJSON(t, ls, "/web/upload/start", body)
}

func partFiles(t *testing.T, ls *liveServer) []string {
	t.Helper()
	entries, err := os.ReadDir(ls.uploads.dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}

func evidenceCount(t *testing.T, db *badger.DB) int {
	t.Helper()
	n := 0
	err := db.View(func(txn *badger.Txn) error {
		opts := badger.DefaultIteratorOptions
		opts.Prefix = []byte(cnst.EviFileNamespace)
		opts.PrefetchValues = false
		it := txn.NewIterator(opts)
		defer it.Close()
		for it.Rewind(); it.Valid(); it.Next() {
			n++
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// countIngests counts the ingests a server runs.
func countIngests(n *atomic.Int32) func(*uploads) {
	return func(m *uploads) {
		next := m.runIngest
		m.runIngest = func(ctx context.Context, db *badger.DB, req ingest.Request) (ingest.Result, error) {
			n.Add(1)
			return next(ctx, db, req)
		}
	}
}

// SRV-2, SRV-3: an upload is received at the offsets the client names, a
// repeated chunk is harmless, a chunk out of place is refused with the
// offset to send from, and finalize stores the file under the client's
// name and returns its evidence. It used to fail after storing the file
// ("metadata failed: must provide a file to save"), leaving it orphaned.
func TestUploadV2(t *testing.T) {
	t.Parallel()
	var ingests atomic.Int32
	ls := uploadServer(t, nil, countIngests(&ingests))
	db := ls.deps.DB
	data, err := os.ReadFile(fixture(t, "fat/fat16"))
	if err != nil {
		t.Fatal(err)
	}
	r := start(t, ls, `C:\case\disk.img`, "Windows_FAT", plainHash(db, data), int64(len(data)))
	id := r.str("upload_id")
	if r.status != 200 || id == "" || r.num("chunk_max") != maxChunk || r.num("size") != int64(len(data)) {
		t.Fatalf("start: %d %v", r.status, r.body)
	}
	third := int64(len(data) / 3)
	if r := chunk(t, ls, id, 0, data[:third]); r.status != 200 || r.num("offset") != third || r.header.Get("Upload-Offset") != strconv.FormatInt(third, 10) {
		t.Fatalf("chunk 1: %d %v", r.status, r.body)
	}
	// The same chunk again: the client did not see the answer.
	if r := chunk(t, ls, id, 0, data[:third]); r.status != 200 || r.num("offset") != third {
		t.Fatalf("chunk 1 again: %d %v", r.status, r.body)
	}
	// A chunk past the end the session holds.
	if r := chunk(t, ls, id, 2*third, data[2*third:]); r.status != http.StatusConflict || r.num("offset") != third || r.header.Get("Upload-Offset") != strconv.FormatInt(third, 10) {
		t.Fatalf("chunk out of place: %d %v", r.status, r.body)
	}
	// Overlapping what it holds and going on past it.
	if r := chunk(t, ls, id, third-1, data[third-1:2*third]); r.status != http.StatusConflict {
		t.Fatalf("overlapping chunk: %d %v", r.status, r.body)
	}
	if r := chunk(t, ls, id, -1, data[third:2*third]); r.status != http.StatusBadRequest {
		t.Fatalf("v2 chunk without Upload-Offset: %d %v", r.status, r.body)
	}
	// Finalizing before every byte arrived.
	if r := postJSON(t, ls, "/web/upload/finalize", map[string]string{"upload_id": id}); r.status != http.StatusConflict || r.num("offset") != third {
		t.Fatalf("early finalize: %d %v", r.status, r.body)
	}
	if r := chunk(t, ls, id, third, data[third:2*third]); r.status != 200 {
		t.Fatalf("chunk 2: %d %v", r.status, r.body)
	}
	if r := chunk(t, ls, id, 2*third, data[2*third:]); r.status != 200 || r.num("offset") != int64(len(data)) {
		t.Fatalf("chunk 3: %d %v", r.status, r.body)
	}
	if r := postJSON(t, ls, "/web/upload/status", map[string]string{"upload_id": id}); r.status != 200 || r.str("state") != stateReceiving || r.num("offset") != int64(len(data)) {
		t.Fatalf("status: %d %v", r.status, r.body)
	}

	fin := postJSON(t, ls, "/web/upload/finalize", map[string]string{"upload_id": id})
	if fin.status != 200 || fin.body["done"] != true || fin.str("outcome") != "stored" {
		t.Fatalf("finalize: %d %v", fin.status, fin.body)
	}
	ef, _ := fin.body["evi_file"].(map[string]any)
	key, _ := base64.StdEncoding.DecodeString(ef["file_id"].(string))
	e, err := dbio.GetEvidenceFile(key, db)
	if err != nil {
		t.Fatalf("the evidence finalize names: %v", err)
	}
	if e.Name != "disk.img" || e.Source == nil || e.Source.Members[0].Path != `C:\case\disk.img` || e.EvidenceType != "windows_fat" || e.Size != int64(len(data)) {
		t.Errorf("evidence %q from %+v, type %q, size %d", e.Name, e.Source, e.EvidenceType, e.Size)
	}
	if ef["file_path"] != `C:\case\disk.img` || len(ef["chunk_map"].(map[string]any)) == 0 || fin.str("index_state") != string(structs.IndexComplete) {
		t.Errorf("evi_file %v, index %q", ef, fin.str("index_state"))
	}
	if files := partFiles(t, ls); len(files) != 0 {
		t.Errorf("left in uploads: %v", files)
	}
	// Finalize again, and status: the same result, and no second ingest.
	again := postJSON(t, ls, "/web/upload/finalize", map[string]string{"upload_id": id})
	if again.status != 200 || again.body["evi_file"].(map[string]any)["file_id"] != ef["file_id"] {
		t.Errorf("finalize again: %d %v", again.status, again.body)
	}
	st := postJSON(t, ls, "/web/upload/status", map[string]string{"upload_id": id})
	if st.str("state") != stateDone || st.body["result"] == nil {
		t.Errorf("status after finalize: %v", st.body)
	}
	if n := ingests.Load(); n != 1 {
		t.Errorf("%d ingests", n)
	}
	// Chunks after finalize are refused.
	if r := chunk(t, ls, id, int64(len(data)), []byte("x")); r.status != http.StatusConflict {
		t.Errorf("chunk after finalize: %d %v", r.status, r.body)
	}
}

// Upload v1 is gone: a start without size opens no session, and a chunk
// without Upload-Offset is refused.
func TestUploadNeedsSizeAndOffset(t *testing.T) {
	t.Parallel()
	ls := uploadServer(t, nil, nil)
	db := ls.deps.DB
	data := []byte("no size")
	r := start(t, ls, "/home/x/v1.bin", "", plainHash(db, data), -1)
	if r.status != http.StatusBadRequest || !strings.Contains(r.str("error"), "size is required") || r.str("upload_id") != "" {
		t.Fatalf("start without size: %d %v", r.status, r.body)
	}
	if files := partFiles(t, ls); len(files) != 0 {
		t.Errorf("a start without size left %v", files)
	}
	id := start(t, ls, "/home/x/v2.bin", "", plainHash(db, data), int64(len(data))).str("upload_id")
	if r := chunk(t, ls, id, -1, data); r.status != http.StatusBadRequest || !strings.Contains(r.str("error"), "Upload-Offset is required") {
		t.Errorf("chunk without Upload-Offset: %d %v", r.status, r.body)
	}
	if st := postJSON(t, ls, "/web/upload/status", map[string]string{"upload_id": id}); st.num("offset") != 0 {
		t.Errorf("a refused chunk was written: %v", st.body)
	}
}

// SRV-3: the server checks the bytes against the client's hash before
// storing anything.
func TestUploadHashMismatchStoresNothing(t *testing.T) {
	t.Parallel()
	ls := uploadServer(t, nil, nil)
	db := ls.deps.DB
	data := []byte("what the client meant to send")
	r := start(t, ls, "/x/f.bin", "", plainHash(db, []byte("something else")), int64(len(data)))
	id := r.str("upload_id")
	if r := chunk(t, ls, id, 0, data); r.status != 200 {
		t.Fatalf("chunk: %d %v", r.status, r.body)
	}
	fin := postJSON(t, ls, "/web/upload/finalize", map[string]string{"upload_id": id})
	if fin.status != http.StatusUnprocessableEntity || !strings.Contains(fin.str("error"), plainHash(db, data)) {
		t.Fatalf("finalize: %d %v", fin.status, fin.body)
	}
	if n := evidenceCount(t, db); n != 0 {
		t.Errorf("%d evidence stored", n)
	}
	if files := partFiles(t, ls); len(files) != 0 {
		t.Errorf("left in uploads: %v", files)
	}
	if st := postJSON(t, ls, "/web/upload/status", map[string]string{"upload_id": id}); st.str("state") != stateFailed {
		t.Errorf("status: %v", st.body)
	}
	// A hash of the wrong length is refused at start.
	if r := start(t, ls, "/x/f.bin", "", base64.StdEncoding.EncodeToString([]byte("short")), 1); r.status != http.StatusBadRequest {
		t.Errorf("short hash: %d %v", r.status, r.body)
	}
}

// SRV-2: sessions, sizes, chunks and free space are bounded.
func TestUploadLimits(t *testing.T) {
	t.Parallel()
	lim := DefaultUploadLimits()
	lim.MaxSessions = 2
	lim.MaxSize = 1 << 20
	var free atomic.Uint64
	free.Store(1 << 40)
	ls := uploadServer(t, &lim, func(m *uploads) {
		m.free = func(string) (uint64, error) { return free.Load(), nil }
	})
	db := ls.deps.DB
	h := plainHash(db, []byte("x"))
	a := start(t, ls, "/a", "", h, 10)
	b := start(t, ls, "/b", "", h, 10)
	if a.status != 200 || b.status != 200 {
		t.Fatalf("two sessions: %d %d", a.status, b.status)
	}
	if r := start(t, ls, "/c", "", h, 10); r.status != http.StatusTooManyRequests || r.header.Get("Retry-After") == "" {
		t.Errorf("a third session: %d %v", r.status, r.body)
	}
	if r := postJSON(t, ls, "/web/upload/abort", map[string]string{"upload_id": b.str("upload_id")}); r.status != 200 {
		t.Fatalf("abort: %d %v", r.status, r.body)
	}
	if r := start(t, ls, "/too-big", "", h, lim.MaxSize+1); r.status != http.StatusRequestEntityTooLarge {
		t.Errorf("over the size cap: %d %v", r.status, r.body)
	}
	free.Store(100)
	if r := start(t, ls, "/no-room", "", h, 1000); r.status != http.StatusInsufficientStorage {
		t.Errorf("without free space: %d %v", r.status, r.body)
	}
	free.Store(1 << 40)
	id := a.str("upload_id")
	if r := chunk(t, ls, id, 0, make([]byte, 11)); r.status != http.StatusRequestEntityTooLarge || r.num("offset") != 0 {
		t.Errorf("past the declared size: %d %v", r.status, r.body)
	}
	if r := chunk(t, ls, id, 0, make([]byte, maxChunk+1)); r.status != http.StatusRequestEntityTooLarge {
		t.Errorf("a chunk over chunk_max: %d %v", r.status, r.body)
	}
	if r := chunk(t, ls, "no-such-upload", 0, []byte("x")); r.status != http.StatusNotFound {
		t.Errorf("unknown session: %d", r.status)
	}
	for name, body := range map[string]map[string]any{
		"negative size": {"file_path": "/x", "file_hash": h, "size": -1},
		"no path":       {"file_hash": h, "size": 1},
		"bad type":      {"file_path": "/x", "file_hash": h, "file_type": "a b", "size": 1},
		"no hash":       {"file_path": "/x", "size": 1},
	} {
		if r := postJSON(t, ls, "/web/upload/start", body); r.status != http.StatusBadRequest {
			t.Errorf("%s: %d %v", name, r.status, r.body)
		}
	}
}

// SRV-2: a full disk truncates the file back to what was acknowledged and
// answers 507; the chunk can then be sent again.
func TestUploadDiskFull(t *testing.T) {
	t.Parallel()
	var fail atomic.Bool
	ls := uploadServer(t, nil, func(m *uploads) {
		m.writeAt = func(f *os.File, p []byte, off int64) (int, error) {
			if fail.Load() {
				n, _ := f.WriteAt(p[:len(p)/2], off)
				return n, testutil.ErrNoSpace()
			}
			return f.WriteAt(p, off)
		}
	})
	db := ls.deps.DB
	data := bytes.Repeat([]byte("0123456789"), 1000)
	r := start(t, ls, "/x/full.bin", "", plainHash(db, data), int64(len(data)))
	id := r.str("upload_id")
	if r := chunk(t, ls, id, 0, data[:4000]); r.status != 200 {
		t.Fatal(r.body)
	}
	fail.Store(true)
	if r := chunk(t, ls, id, 4000, data[4000:]); r.status != http.StatusInsufficientStorage || r.num("offset") != 4000 {
		t.Fatalf("a write on a full disk: %d %v", r.status, r.body)
	}
	info, err := os.Stat(filepath.Join(ls.uploads.dir, id+".part"))
	if err != nil || info.Size() != 4000 {
		t.Fatalf("part file after the failed write: %v, %d bytes", err, info.Size())
	}
	fail.Store(false)
	if r := chunk(t, ls, id, 4000, data[4000:]); r.status != 200 {
		t.Fatalf("retry: %d %v", r.status, r.body)
	}
	if fin := postJSON(t, ls, "/web/upload/finalize", map[string]string{"upload_id": id}); fin.status != 200 {
		t.Fatalf("finalize: %d %v", fin.status, fin.body)
	}
}

// SRV-2: finalize runs once, however many calls race for it.
func TestUploadFinalizeOnce(t *testing.T) {
	t.Parallel()
	var ingests atomic.Int32
	ls := uploadServer(t, nil, countIngests(&ingests))
	db := ls.deps.DB
	data := bytes.Repeat([]byte("once "), 10_000)
	id := start(t, ls, "/x/once.bin", "", plainHash(db, data), int64(len(data))).str("upload_id")
	if r := chunk(t, ls, id, 0, data); r.status != 200 {
		t.Fatal(r.body)
	}
	var wg sync.WaitGroup
	ids := make([]string, 8)
	for i := range ids {
		wg.Add(1)
		go func() {
			defer wg.Done()
			b, _ := json.Marshal(map[string]string{"upload_id": id})
			resp, err := http.Post(ls.url+"/web/upload/finalize", "application/json", bytes.NewReader(b))
			if err != nil {
				return
			}
			defer resp.Body.Close()
			var body map[string]any
			_ = json.NewDecoder(resp.Body).Decode(&body)
			if ef, ok := body["evi_file"].(map[string]any); ok && resp.StatusCode == 200 {
				ids[i], _ = ef["file_id"].(string)
			}
		}()
	}
	wg.Wait()
	for i, got := range ids {
		if got == "" || got != ids[0] {
			t.Fatalf("finalize %d gave %q, the first %q", i, got, ids[0])
		}
	}
	if n := ingests.Load(); n != 1 {
		t.Fatalf("%d ingests for one upload", n)
	}
}

// A finalize that outlasts FinalizeWait answers 202; status and a later
// finalize give the result.
func TestUploadFinalizePending(t *testing.T) {
	t.Parallel()
	lim := DefaultUploadLimits()
	lim.FinalizeWait = 100 * time.Millisecond
	release := make(chan struct{})
	ls := uploadServer(t, &lim, func(m *uploads) {
		next := m.runIngest
		m.runIngest = func(ctx context.Context, db *badger.DB, req ingest.Request) (ingest.Result, error) {
			<-release
			return next(ctx, db, req)
		}
	})
	db := ls.deps.DB
	data := []byte("slow to store")
	id := start(t, ls, "/x/slow.bin", "", plainHash(db, data), int64(len(data))).str("upload_id")
	chunk(t, ls, id, 0, data)
	if r := postJSON(t, ls, "/web/upload/finalize", map[string]string{"upload_id": id}); r.status != http.StatusAccepted || r.str("state") != stateFinalizing {
		t.Fatalf("finalize: %d %v", r.status, r.body)
	}
	if r := postJSON(t, ls, "/web/upload/abort", map[string]string{"upload_id": id}); r.status != http.StatusConflict {
		t.Errorf("abort while finalizing: %d %v", r.status, r.body)
	}
	close(release)
	deadline := time.Now().Add(10 * time.Second)
	for {
		st := postJSON(t, ls, "/web/upload/status", map[string]string{"upload_id": id})
		if st.str("state") == stateDone {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("still %v", st.body)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if r := postJSON(t, ls, "/web/upload/finalize", map[string]string{"upload_id": id}); r.status != 200 || r.str("outcome") != "stored" {
		t.Fatalf("finalize after: %d %v", r.status, r.body)
	}
}

// SRV-2: a session ends 30 minutes after its last chunk, and 24 hours after
// it started however active; a finished one is forgotten after DoneTTL. It
// used to end 30 minutes after it started, active or not.
func TestUploadExpiry(t *testing.T) {
	t.Parallel()
	clock := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	lim := DefaultUploadLimits()
	lim.ReapInterval = time.Hour
	ls := uploadServer(t, &lim, func(m *uploads) { m.now = clock.Now })
	db := ls.deps.DB
	h := plainHash(db, []byte("x"))

	idle := start(t, ls, "/idle", "", h, 100).str("upload_id")
	busy := start(t, ls, "/busy", "", h, 100).str("upload_id")
	asking := start(t, ls, "/asking", "", h, 100).str("upload_id")
	for i := int64(0); i < 4; i++ {
		clock.Add(20 * time.Minute)
		if r := chunk(t, ls, busy, i, []byte("b")); r.status != 200 {
			t.Fatalf("busy chunk %d: %d %v", i, r.status, r.body)
		}
		if r := postJSON(t, ls, "/web/upload/status", map[string]string{"upload_id": asking}); r.status != 200 {
			t.Fatalf("status %d: %d %v", i, r.status, r.body)
		}
		ls.uploads.reap()
	}
	if r := chunk(t, ls, idle, 0, []byte("i")); r.status != http.StatusNotFound {
		t.Errorf("idle session after 80 minutes: %d", r.status)
	}
	if r := chunk(t, ls, asking, 0, []byte("a")); r.status != 200 {
		t.Errorf("a session whose client asked for its status: %d %v", r.status, r.body)
	}
	if r := chunk(t, ls, busy, 4, []byte("b")); r.status != 200 {
		t.Errorf("busy session after 80 minutes: %d %v", r.status, r.body)
	}
	for i := int64(5); i < 100 && clock.Now().Before(time.Unix(1_700_000_000, 0).Add(24*time.Hour)); i++ {
		clock.Add(20 * time.Minute)
		chunk(t, ls, busy, i, []byte("b"))
	}
	clock.Add(20 * time.Minute)
	ls.uploads.reap()
	if r := chunk(t, ls, busy, 0, []byte("b")); r.status != http.StatusNotFound {
		t.Errorf("busy session after 24 hours: %d", r.status)
	}
	if files := partFiles(t, ls); len(files) != 0 {
		t.Errorf("left in uploads: %v", files)
	}

	data := []byte("done")
	done := start(t, ls, "/done", "", plainHash(db, data), int64(len(data))).str("upload_id")
	chunk(t, ls, done, 0, data)
	postJSON(t, ls, "/web/upload/finalize", map[string]string{"upload_id": done})
	clock.Add(lim.DoneTTL + time.Second)
	ls.uploads.reap()
	if r := postJSON(t, ls, "/web/upload/status", map[string]string{"upload_id": done}); r.status != http.StatusNotFound {
		t.Errorf("finished session after DoneTTL: %d %v", r.status, r.body)
	}
}

func TestUploadAbort(t *testing.T) {
	t.Parallel()
	ls := uploadServer(t, nil, nil)
	h := plainHash(ls.deps.DB, []byte("x"))
	id := start(t, ls, "/x", "", h, 10).str("upload_id")
	chunk(t, ls, id, 0, []byte("12345"))
	if r := postJSON(t, ls, "/web/upload/abort", map[string]string{"upload_id": id}); r.status != 200 {
		t.Fatalf("abort: %d %v", r.status, r.body)
	}
	if files := partFiles(t, ls); len(files) != 0 {
		t.Errorf("left in uploads: %v", files)
	}
	if r := chunk(t, ls, id, 5, []byte("6")); r.status != http.StatusNotFound {
		t.Errorf("chunk after abort: %d", r.status)
	}
}

// Part files: created for the session alone, readable by the owner only,
// and swept when the server starts.
func TestUploadFiles(t *testing.T) {
	t.Parallel()
	h := openDB(t)
	dir := filepath.Join(h.Root, cnst.UploadsDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	v7, _ := uuid.NewV7()
	left := []string{uuid.NewString() + ".part", v7.String(), "keep.txt"}
	for _, n := range left {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	ls := startServer(t, Config{}, Deps{DB: h.DB, Root: h.Root}, nil)
	if files := partFiles(t, ls); len(files) != 1 || files[0] != "keep.txt" {
		t.Errorf("after start, uploads holds %v", files)
	}
	if !strings.Contains(strings.Join(ls.Notes, "\n"), "removed 2 unfinished uploads") {
		t.Errorf("notes %q", ls.Notes)
	}
	id := start(t, ls, "/x", "", plainHash(h.DB, []byte("x")), 1).str("upload_id")
	info, err := os.Stat(filepath.Join(dir, id+".part"))
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Errorf("part file mode %v", info.Mode().Perm())
	}
}

// SRV-4: StreamFile goes through the same sessions. A client that goes
// away mid-stream leaves no part file and no evidence.
func TestStreamFile(t *testing.T) {
	t.Parallel()
	ls := uploadServer(t, nil, nil)
	db := ls.deps.DB
	c := pbconnect.NewDuesServiceClient(http.DefaultClient, ls.url)
	data := bytes.Repeat([]byte("streamed "), 100_000)

	stream := c.StreamFile(context.Background())
	if err := stream.Send(&pb.StreamFileReq{Payload: &pb.StreamFileReq_FileMeta{FileMeta: &pb.StreamFileMeta{
		FilePath: "/client/streamed.bin", FileHash: plainHash(db, data), Size: int64(len(data)),
	}}}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < len(data); i += 256 << 10 {
		if err := stream.Send(&pb.StreamFileReq{Payload: &pb.StreamFileReq_File{File: data[i:min(i+256<<10, len(data))]}}); err != nil {
			t.Fatal(err)
		}
	}
	res, err := stream.CloseAndReceive()
	if err != nil {
		t.Fatal(err)
	}
	key, _ := base64.StdEncoding.DecodeString(res.Msg.EviFile.GetFileId())
	e, err := dbio.GetEvidenceFile(key, db)
	if err != nil || e.Name != "streamed.bin" || e.Size != int64(len(data)) || res.Msg.EviFile.FilePath != "/client/streamed.bin" {
		t.Fatalf("stored %+v (%v), reply %+v", e, err, res.Msg.EviFile)
	}

	// A client that goes away.
	ctx, cancel := context.WithCancel(context.Background())
	broken := c.StreamFile(ctx)
	if err := broken.Send(&pb.StreamFileReq{Payload: &pb.StreamFileReq_FileMeta{FileMeta: &pb.StreamFileMeta{
		FilePath: "/client/broken.bin", FileHash: plainHash(db, []byte("never")),
	}}}); err != nil {
		t.Fatal(err)
	}
	if err := broken.Send(&pb.StreamFileReq{Payload: &pb.StreamFileReq_File{File: data[:1000]}}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for len(partFiles(t, ls)) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	_, _ = broken.CloseAndReceive()
	for len(partFiles(t, ls)) != 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if files := partFiles(t, ls); len(files) != 0 {
		t.Errorf("a broken stream left %v", files)
	}
	if n := evidenceCount(t, db); n != 1 {
		t.Errorf("%d evidence after a broken stream", n)
	}

	// The first message must carry the metadata; a wrong hash is data loss.
	bad := c.StreamFile(context.Background())
	_ = bad.Send(&pb.StreamFileReq{Payload: &pb.StreamFileReq_File{File: []byte("x")}})
	if _, err := bad.CloseAndReceive(); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("no metadata first: %v", err)
	}
	wrong := c.StreamFile(context.Background())
	_ = wrong.Send(&pb.StreamFileReq{Payload: &pb.StreamFileReq_FileMeta{FileMeta: &pb.StreamFileMeta{FilePath: "/w", FileHash: plainHash(db, []byte("y"))}}})
	_ = wrong.Send(&pb.StreamFileReq{Payload: &pb.StreamFileReq_File{File: []byte("z")}})
	if _, err := wrong.CloseAndReceive(); connect.CodeOf(err) != connect.CodeDataLoss {
		t.Errorf("a wrong hash: %v", err)
	}
}

// A StreamFile without size is checked at start against nothing, so the cap
// on one upload applies to the bytes it sends: up to the cap is stored, one
// byte more is refused and leaves nothing behind.
func TestStreamFileWithoutSizeIsCapped(t *testing.T) {
	t.Parallel()
	lim := DefaultUploadLimits()
	lim.MaxSize = 1 << 20
	ls := uploadServer(t, &lim, nil)
	db := ls.deps.DB
	c := pbconnect.NewDuesServiceClient(http.DefaultClient, ls.url)
	send := func(data []byte) error {
		stream := c.StreamFile(context.Background())
		_ = stream.Send(&pb.StreamFileReq{Payload: &pb.StreamFileReq_FileMeta{FileMeta: &pb.StreamFileMeta{
			FilePath: fmt.Sprintf("/client/%d.bin", len(data)), FileHash: plainHash(db, data),
		}}})
		for i := 0; i < len(data); i += 256 << 10 {
			// A refused message ends the stream; its error comes with the reply.
			if stream.Send(&pb.StreamFileReq{Payload: &pb.StreamFileReq_File{File: data[i:min(i+256<<10, len(data))]}}) != nil {
				break
			}
		}
		_, err := stream.CloseAndReceive()
		return err
	}
	if err := send(bytes.Repeat([]byte("a"), int(lim.MaxSize))); err != nil {
		t.Fatalf("a stream of the cap: %v", err)
	}
	if err := send(bytes.Repeat([]byte("b"), int(lim.MaxSize)+1)); connect.CodeOf(err) != connect.CodeOutOfRange {
		t.Errorf("a stream past the cap: %v", err)
	}
	if files := partFiles(t, ls); len(files) != 0 {
		t.Errorf("a refused stream left %v", files)
	}
	if n := evidenceCount(t, db); n != 1 {
		t.Errorf("%d evidence, want 1", n)
	}
}

// Content already stored is recorded as an alias; the client's file type
// does not change the evidence that holds it.
func TestUploadOfStoredContent(t *testing.T) {
	t.Parallel()
	ls := uploadServer(t, nil, nil)
	db := ls.deps.DB
	path := fixture(t, "fat/fat12")
	first := storeFile(t, db, path)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	before, _ := dbio.GetEvidenceFile(first.EvidenceID, db)
	id := start(t, ls, "/elsewhere/copy.img", "other_type", plainHash(db, data), int64(len(data))).str("upload_id")
	chunk(t, ls, id, 0, data)
	fin := postJSON(t, ls, "/web/upload/finalize", map[string]string{"upload_id": id})
	if fin.status != 200 || fin.str("outcome") != "aliased" || fin.body["evi_file"].(map[string]any)["file_id"] != evidenceID(first.EvidenceID) {
		t.Fatalf("finalize: %d %v", fin.status, fin.body)
	}
	after, _ := dbio.GetEvidenceFile(first.EvidenceID, db)
	if after.EvidenceType != before.EvidenceType || after.Name != before.Name {
		t.Errorf("the stored evidence changed: type %q to %q, name %q to %q", before.EvidenceType, after.EvidenceType, before.Name, after.Name)
	}
	aliases, _ := dbio.Aliases(db, first.EvidenceID)
	found := false
	for _, a := range aliases {
		found = found || a.Path == "/elsewhere/copy.img"
	}
	if !found {
		t.Errorf("aliases %+v", aliases)
	}
	if n := evidenceCount(t, db); n != 1 {
		t.Errorf("%d evidence", n)
	}
}

// TestConcurrentUploads: clients upload at once, three of one content and
// three of others. Every upload is stored or aliased, each content is
// stored once, and the clients of one content get the same evidence.
func TestConcurrentUploads(t *testing.T) {
	t.Parallel()
	ls := uploadServer(t, nil, nil)
	db := ls.deps.DB
	shared := bytes.Repeat([]byte("shared content "), 20_000)
	contents := [][]byte{shared, shared, shared}
	for i := range 3 {
		contents = append(contents, bytes.Repeat([]byte(fmt.Sprintf("distinct %d ", i)), 20_000))
	}
	ids := make([]string, len(contents))
	errs := make([]error, len(contents))
	var wg sync.WaitGroup
	for i, data := range contents {
		wg.Go(func() {
			path := fmt.Sprintf("/client-%d/file.bin", i)
			b, _ := json.Marshal(map[string]any{"file_path": path, "file_hash": plainHash(db, data), "size": len(data)})
			resp, err := http.Post(ls.url+"/web/upload/start", "application/json", bytes.NewReader(b))
			if err != nil {
				errs[i] = err
				return
			}
			var started map[string]any
			_ = json.NewDecoder(resp.Body).Decode(&started)
			resp.Body.Close()
			id, _ := started["upload_id"].(string)
			req, _ := http.NewRequest(http.MethodPost, ls.url+"/web/upload/chunk", bytes.NewReader(data))
			req.Header.Set("Upload-Id", id)
			req.Header.Set("Upload-Offset", "0")
			if resp, err = http.DefaultClient.Do(req); err != nil {
				errs[i] = err
				return
			}
			resp.Body.Close()
			b, _ = json.Marshal(map[string]string{"upload_id": id})
			if resp, err = http.Post(ls.url+"/web/upload/finalize", "application/json", bytes.NewReader(b)); err != nil {
				errs[i] = err
				return
			}
			defer resp.Body.Close()
			var body map[string]any
			_ = json.NewDecoder(resp.Body).Decode(&body)
			ef, _ := body["evi_file"].(map[string]any)
			if resp.StatusCode != http.StatusOK || ef == nil {
				errs[i] = fmt.Errorf("finalize: %d %v", resp.StatusCode, body)
				return
			}
			ids[i], _ = ef["file_id"].(string)
		})
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("upload %d: %v", i, err)
		}
	}
	if ids[0] == "" || ids[1] != ids[0] || ids[2] != ids[0] {
		t.Fatalf("the uploads of one content got evidence %q", ids[:3])
	}
	if ids[3] == ids[4] || ids[4] == ids[5] || ids[3] == ids[0] {
		t.Fatalf("distinct contents share evidence: %q", ids)
	}
	if n := evidenceCount(t, db); n != 4 {
		t.Fatalf("%d evidence records, want 4", n)
	}
	if parts := partFiles(t, ls); len(parts) != 0 {
		t.Fatalf("part files left: %v", parts)
	}
}

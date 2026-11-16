package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"indicer/internal/diskspace"
	"indicer/internal/testutil"
	"indicer/lib/cnst"
	"indicer/lib/dbopen"
	"indicer/lib/dbpackage"
	"indicer/lib/fsx"
	"indicer/lib/graph/signer"
	"indicer/lib/ingest"
	"indicer/lib/util"
	"indicer/pb"
	"indicer/pb/pbconnect"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

const (
	exportStartPath  = "/web/export/start"
	exportStatusPath = "/web/export/status"
	exportCancelPath = "/web/export/cancel"

	exporterLabel = "exporter"
	otherLabel    = "other"
	readerLabel   = "reader"

	// exportEvidenceSize is the size of the one evidence an export test
	// database holds.
	exportEvidenceSize = 64 << 10
	// stopBound bounds a clean stop of a test server: its ShutdownGrace and
	// IdleWait, with room for a slow host.
	stopBound = 5 * time.Second
	// endlessSize is a download no client reads to its end.
	endlessSize = 1 << 40
	// readsHeld bounds the downloads a gatedOpen holds at once.
	readsHeld = 16
	// h2Resets is how many downloads an HTTP/2 client resets, each a chance
	// for the cut to race the handler's return.
	h2Resets = 20
)

var (
	exporterToken = testToken40('e')
	otherToken    = testToken40('o')
	readerToken   = testToken40('r')
)

// exportServer serves a database holding one evidence to three tokens: the
// exporter may do anything, other may only export, and reader may not
// export. edit may change the configuration; setup runs before the server
// listens.
func exportServer(t *testing.T, edit func(*Config), setup func(*Server)) (*liveServer, *observer.ObservedLogs) {
	t.Helper()
	h := openDB(t)
	if err := util.EnsureBlobPath(h.Root); err != nil {
		t.Fatal(err)
	}
	storeFile(t, h.DB, testutil.WriteRandomFile(t, t.TempDir(), "evidence.bin", 7, exportEvidenceSize))
	return exportServerOn(t, h, edit, setup)
}

// exportServerOn is exportServer on the database h.
func exportServerOn(t *testing.T, h *dbopen.Handle, edit func(*Config), setup func(*Server)) (*liveServer, *observer.ObservedLogs) {
	t.Helper()
	cfg := Config{Limits: testLimits(), Tokens: []Credential{
		mustCredential(t, exporterToken, ScopeAll, exporterLabel),
		mustCredential(t, otherToken, ScopeExport, otherLabel),
		mustCredential(t, readerToken, DefaultScopes, readerLabel),
	}}
	if edit != nil {
		edit(&cfg)
	}
	core, logs := observer.New(zap.InfoLevel)
	return startServer(t, cfg, Deps{Handle: h, Log: zap.New(core)}, setup), logs
}

// asExporter is a request context of the exporter's, for calls to the
// export manager made without HTTP.
func asExporter(t *testing.T) context.Context {
	return context.WithValue(t.Context(), credentialKey, mustCredential(t, exporterToken, ScopeAll, exporterLabel))
}

func exportCall(t *testing.T, ls *liveServer, token, path string, body any) reply {
	t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	return post(t, bearer(newReq(t, http.MethodPost, ls.url+path, bytes.NewReader(b)), token))
}

func startExport(t *testing.T, ls *liveServer, token string, withoutDerived bool) reply {
	t.Helper()
	return exportCall(t, ls, token, exportStartPath, map[string]bool{"without_derived": withoutDerived})
}

func exportStatusOf(t *testing.T, ls *liveServer, token, id string) reply {
	t.Helper()
	return exportCall(t, ls, token, exportStatusPath, exportIDReq{ExportID: id})
}

func cancelExport(t *testing.T, ls *liveServer, token, id string) reply {
	t.Helper()
	return exportCall(t, ls, token, exportCancelPath, exportIDReq{ExportID: id})
}

// callAsync posts body to path of the server at base from its own
// goroutine, which must not stop the test, and sends the answer; a failed
// request sends status 0.
func callAsync(base, token, path string, body any) <-chan reply {
	out := make(chan reply, 1)
	go func() {
		b, err := json.Marshal(body)
		if err != nil {
			out <- reply{body: map[string]any{"error": err.Error()}}
			return
		}
		req, err := http.NewRequest(http.MethodPost, base+path, bytes.NewReader(b))
		if err != nil {
			out <- reply{body: map[string]any{"error": err.Error()}}
			return
		}
		resp, err := http.DefaultClient.Do(bearer(req, token))
		if err != nil {
			out <- reply{body: map[string]any{"error": err.Error()}}
			return
		}
		defer resp.Body.Close()
		r := reply{status: resp.StatusCode, header: resp.Header}
		if err := json.NewDecoder(resp.Body).Decode(&r.body); err != nil {
			r.body = map[string]any{"error": err.Error()}
		}
		out <- r
	}()
	return out
}

// awaitExport asks for the status of export id until it is in one of
// states.
func awaitExport(t *testing.T, ls *liveServer, token, id string, states ...string) reply {
	t.Helper()
	deadline := time.Now().Add(queueWait)
	for {
		r := exportStatusOf(t, ls, token, id)
		if r.status == http.StatusOK && slices.Contains(states, r.str("state")) {
			return r
		}
		if time.Now().After(deadline) {
			t.Fatalf("export %s never %v: %d %v", id, states, r.status, r.body)
		}
		time.Sleep(pollEvery)
	}
}

// exportIdle waits until no export runs, its resume included, so another
// may start.
func exportIdle(t *testing.T, ls *liveServer) {
	t.Helper()
	ex := ls.exports
	until(t, "the export and its resume to end", func() bool {
		ex.mu.Lock()
		defer ex.mu.Unlock()
		return !ex.busy
	})
}

// exportDone makes a package as the exporter and returns its id and status
// once the database has resumed.
func exportDone(t *testing.T, ls *liveServer, withoutDerived bool) (string, reply) {
	t.Helper()
	r := startExport(t, ls, exporterToken, withoutDerived)
	id := r.str("export_id")
	if r.status != http.StatusAccepted || id == "" {
		t.Fatalf("start: %d %v", r.status, r.body)
	}
	st := awaitExport(t, ls, exporterToken, id, exportStateDone, exportStateFailed, exportStateCancelled)
	if st.str("state") != exportStateDone {
		t.Fatalf("export %s: %v", id, st.body)
	}
	exportIdle(t, ls)
	return id, st
}

// downloadReq is a request for the package of export id.
func downloadReq(t *testing.T, ls *liveServer, token, id, method string) *http.Request {
	t.Helper()
	return bearer(newReq(t, method, ls.url+downloadPath+id, nil), token)
}

func download(t *testing.T, ls *liveServer, token, id string, h http.Header, method string) (*http.Response, []byte) {
	t.Helper()
	req := downloadReq(t, ls, token, id, method)
	for k, v := range h {
		req.Header[k] = v
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp, body
}

// errorOf is the error a JSON error body names.
func errorOf(body []byte) string {
	var eb errorBody
	_ = json.Unmarshal(body, &eb)
	return eb.Error
}

// exportFiles are the names in the export folder.
func exportFiles(t *testing.T, ls *liveServer) []string {
	t.Helper()
	entries, err := os.ReadDir(ls.exports.dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func wantNoExportFiles(t *testing.T, ls *liveServer) {
	t.Helper()
	if names := exportFiles(t, ls); len(names) != 0 {
		t.Errorf("the export folder holds %v, want nothing", names)
	}
}

// wantPair checks whether both files of export id are in the folder.
func wantPair(t *testing.T, ls *liveServer, id string, present bool) {
	t.Helper()
	for _, suffix := range []string{dbpackage.Ext, sidecarSuffix} {
		_, err := os.Stat(ls.exports.file(id, suffix))
		if present && err != nil {
			t.Errorf("%s%s: %v", id, suffix, err)
		}
		if !present && !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("%s%s is still there (%v)", id, suffix, err)
		}
	}
}

func downloadsOf(ls *liveServer, id string) int {
	ex := ls.exports
	ex.mu.Lock()
	defer ex.mu.Unlock()
	if j := ex.jobs[id]; j != nil {
		return j.downloads
	}
	return 0
}

// holdExport is an export seam that tells when it is called, then waits to
// be released, or for its context, before it exports. Made after the
// server, its cleanup releases it before the server stops.
type holdExport struct {
	in      chan struct{}
	gate    chan struct{}
	release func()
}

func newHoldExport(t *testing.T) *holdExport {
	h := &holdExport{in: make(chan struct{}, 1), gate: make(chan struct{})}
	h.release = sync.OnceFunc(func() { close(h.gate) })
	t.Cleanup(h.release)
	return h
}

func (h *holdExport) export(ctx context.Context, opts dbpackage.ExportOptions) (dbpackage.ExportResult, error) {
	select {
	case h.in <- struct{}{}:
	default:
	}
	select {
	case <-h.gate:
	case <-ctx.Done():
		return dbpackage.ExportResult{}, ctx.Err()
	}
	return dbpackage.Export(ctx, opts)
}

// wait waits until an export is packaging.
func (h *holdExport) wait(t *testing.T) {
	t.Helper()
	await(t, "the export to package", h.in)
}

// gatedOpen is an open seam; while hold is set, the files it opens hold
// their first read until released. Nothing else ends that wait, so a
// download it holds is not ended by its request.
type gatedOpen struct {
	hold    atomic.Bool
	reading chan struct{}
	gate    chan struct{}
	release func()
	opened  atomic.Int32
}

func newGatedOpen(t *testing.T) *gatedOpen {
	g := &gatedOpen{reading: make(chan struct{}, readsHeld), gate: make(chan struct{})}
	g.release = sync.OnceFunc(func() { close(g.gate) })
	t.Cleanup(g.release)
	return g
}

func (g *gatedOpen) open(name string) (io.ReadSeekCloser, error) {
	f, err := os.Open(name)
	if err != nil {
		return nil, err
	}
	g.opened.Add(1)
	if !g.hold.Load() {
		return f, nil
	}
	return &gatedFile{File: f, g: g}, nil
}

type gatedFile struct {
	*os.File
	g    *gatedOpen
	once sync.Once
}

func (f *gatedFile) Read(p []byte) (int, error) {
	f.once.Do(func() {
		f.g.reading <- struct{}{}
		<-f.g.gate
	})
	return f.File.Read(p)
}

// endlessFile is a package too large for a client to read to its end.
type endlessFile struct {
	off    int64
	served *atomic.Int64
}

func (f *endlessFile) Read(p []byte) (int, error) {
	if f.off >= endlessSize {
		return 0, io.EOF
	}
	n := int(min(int64(len(p)), endlessSize-f.off))
	clear(p[:n])
	f.off += int64(n)
	f.served.Add(int64(n))
	return n, nil
}

func (f *endlessFile) Seek(offset int64, whence int) (int64, error) {
	switch whence {
	case io.SeekStart:
		f.off = offset
	case io.SeekCurrent:
		f.off += offset
	case io.SeekEnd:
		f.off = endlessSize + offset
	}
	return f.off, nil
}

func (f *endlessFile) Close() error { return nil }

// TestExportRoundTrip: an export pauses the database, writes a signed
// package with its evidence counted and without the exports folder, and
// serves it with headers that name its SHA-256; the database then serves
// again, reopened.
func TestExportRoundTrip(t *testing.T) {
	t.Parallel()
	ls, _ := exportServer(t, nil, nil)
	r := startExport(t, ls, exporterToken, false)
	id := r.str("export_id")
	if r.status != http.StatusAccepted || !validExportID(id) || r.str("created_at") == "" ||
		!slices.Contains([]string{exportStatePausing, exportStatePackaging, exportStateDone}, r.str("state")) {
		t.Fatalf("start: %d %v", r.status, r.body)
	}
	st := awaitExport(t, ls, exporterToken, id, exportStateDone)
	exportIdle(t, ls)
	sum := st.str("sha256")
	if st.num("size") <= 0 || len(sum) != 2*sha256.Size || st.str("content_sha256") == "" || st.str("signed_by") != signer.DatabaseActor ||
		st.str("key_id") == "" || st.str("download") != downloadPath+id || st.str("expires_at") == "" ||
		st.num("bytes_written") != st.num("size") || st.num("bytes_expected") < st.num("size") || st.body["without_derived"] != false {
		t.Fatalf("status: %v", st.body)
	}
	wantPair(t, ls, id, true)

	resp, body := download(t, ls, exporterToken, id, nil, http.MethodGet)
	got := sha256.Sum256(body)
	if resp.StatusCode != http.StatusOK || hex.EncodeToString(got[:]) != sum || int64(len(body)) != st.num("size") {
		t.Fatalf("download: %d, %d bytes of sha256 %x, want %s", resp.StatusCode, len(body), got, sum)
	}
	for k, want := range map[string]string{
		"ETag":                   `"sha256-` + sum + `"`,
		"Repr-Digest":            "sha-256=:" + base64.StdEncoding.EncodeToString(got[:]) + ":",
		"Cache-Control":          "no-store",
		"Content-Type":           "application/zip",
		"X-Content-Type-Options": "nosniff",
	} {
		if v := resp.Header.Get(k); v != want {
			t.Errorf("%s %q, want %q", k, v, want)
		}
	}
	if disp, params, err := mime.ParseMediaType(resp.Header.Get("Content-Disposition")); err != nil || disp != "attachment" || params["filename"] != id+dbpackage.Ext {
		t.Errorf("Content-Disposition %q (%v)", resp.Header.Get("Content-Disposition"), err)
	}
	if resp.Header.Get("Last-Modified") == "" {
		t.Error("no Last-Modified")
	}

	saved := filepath.Join(t.TempDir(), id+dbpackage.Ext)
	if err := os.WriteFile(saved, body, 0o600); err != nil {
		t.Fatal(err)
	}
	pkg, err := dbpackage.Open(saved)
	if err != nil {
		t.Fatal(err)
	}
	defer pkg.Close()
	vr, err := pkg.Verify(t.Context(), dbpackage.VerifyOptions{})
	if err != nil || vr.Signature == nil || vr.Signature.Actor != signer.DatabaseActor {
		t.Fatalf("verify: %+v, %v", vr.Signature, err)
	}
	m := pkg.Manifest
	if m.Stats == nil || m.Stats.Evidence != 1 {
		t.Errorf("stats %+v, want the one evidence counted", m.Stats)
	}
	if m.ContentSHA256 != st.str("content_sha256") {
		t.Errorf("content hash %s, status says %s", m.ContentSHA256, st.str("content_sha256"))
	}
	for _, f := range m.Files {
		if strings.HasPrefix(f.Path, cnst.ExportsDir+"/") {
			t.Errorf("the package holds %s", f.Path)
		}
	}
	if slices.Contains(m.Dirs, cnst.ExportsDir) {
		t.Errorf("the package holds the exports folder: %v", m.Dirs)
	}

	if status, body, _ := ready(t, ls); status != http.StatusOK || body != bodyReady {
		t.Errorf("readyz after the export: %d %q", status, body)
	}
	if g := gen(ls); g != 2 {
		t.Errorf("gen %d after the export, want 2", g)
	}
	c := pbconnect.NewDuesServiceClient(h2cClient(t, readerToken), ls.url)
	res, err := c.GetEviFiles(t.Context(), connect.NewRequest(&pb.GetEviFilesReq{}))
	if err != nil || len(res.Msg.EviFile) != 1 {
		t.Errorf("the evidence after the export: %v", err)
	}
}

// TestExportWithoutDerived: without_derived leaves the full-text index and
// the graph out, and the status says so.
func TestExportWithoutDerived(t *testing.T) {
	t.Parallel()
	h := openDB(t)
	if err := util.EnsureBlobPath(h.Root); err != nil {
		t.Fatal(err)
	}
	storeFile(t, h.DB, testutil.WriteRandomFile(t, t.TempDir(), "evidence.bin", 7, exportEvidenceSize))
	for _, d := range []string{cnst.FTSDIR, cnst.GRAPHDIR} {
		p := filepath.Join(h.Root, d, "stand-in.dat")
		if err := os.MkdirAll(filepath.Dir(p), cnst.DirPerm); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("derived"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	ls, _ := exportServerOn(t, h, nil, nil)
	derived := func(id string) []string {
		t.Helper()
		_, body := download(t, ls, exporterToken, id, nil, http.MethodGet)
		saved := filepath.Join(t.TempDir(), id+dbpackage.Ext)
		if err := os.WriteFile(saved, body, 0o600); err != nil {
			t.Fatal(err)
		}
		pkg, err := dbpackage.Open(saved)
		if err != nil {
			t.Fatal(err)
		}
		defer pkg.Close()
		var out []string
		for _, f := range pkg.Manifest.Files {
			if top, _, _ := strings.Cut(f.Path, "/"); top == cnst.FTSDIR || top == cnst.GRAPHDIR {
				out = append(out, f.Path)
			}
		}
		return out
	}
	full, _ := exportDone(t, ls, false)
	if got := derived(full); len(got) == 0 {
		t.Fatal("a whole package holds no full-text index or graph file")
	}
	lean, st := exportDone(t, ls, true)
	if st.body["without_derived"] != true {
		t.Errorf("status %v, want without_derived", st.body)
	}
	if got := derived(lean); len(got) != 0 {
		t.Errorf("without_derived, the package holds %v", got)
	}
}

// TestExportOneAtATime: a second start is refused while an export runs and
// while the database resumes after it, though the first is already done;
// then a new one starts.
func TestExportOneAtATime(t *testing.T) {
	t.Parallel()
	h := newHoldExport(t)
	resuming, resumeGate := make(chan struct{}), make(chan struct{})
	releaseResume := sync.OnceFunc(func() { close(resumeGate) })
	ls, _ := exportServer(t, nil, func(s *Server) {
		s.exports.export = h.export
		orig := s.dbs.resume
		var calls atomic.Int32
		s.dbs.resume = func(held *dbopen.Suspended, ctx context.Context, opts dbopen.ResumeOptions) (*dbopen.Handle, error) {
			if calls.Add(1) == 1 {
				close(resuming)
				<-resumeGate
			}
			return orig(held, ctx, opts)
		}
	})
	t.Cleanup(releaseResume)
	first := startExport(t, ls, exporterToken, false)
	id := first.str("export_id")
	if first.status != http.StatusAccepted {
		t.Fatalf("start: %d %v", first.status, first.body)
	}
	h.wait(t)
	if r := startExport(t, ls, exporterToken, false); r.status != http.StatusConflict || r.str("error") != msgExportRunning {
		t.Errorf("a start while one packages: %d %v", r.status, r.body)
	}
	if st := exportStatusOf(t, ls, exporterToken, id); st.status != http.StatusOK || st.str("state") != exportStatePackaging {
		t.Errorf("the first export: %d %v", st.status, st.body)
	}
	h.release()
	await(t, "the resume", resuming)
	awaitExport(t, ls, exporterToken, id, exportStateDone)
	if r := startExport(t, ls, exporterToken, false); r.status != http.StatusConflict || r.str("error") != msgExportRunning {
		t.Errorf("a start while the database resumes: %d %v", r.status, r.body)
	}
	releaseResume()
	exportIdle(t, ls)
	exportDone(t, ls, false)
}

// TestExportScopes: only an export token may export, and only its own
// exports; another's are unknown to it.
func TestExportScopes(t *testing.T) {
	t.Parallel()
	ls, logs := exportServer(t, nil, nil)
	id, _ := exportDone(t, ls, false)
	refused := func(what string, status int, challenge string) {
		t.Helper()
		if status != http.StatusForbidden || !strings.Contains(challenge, `error="insufficient_scope", scope="export"`) {
			t.Errorf("reader %s: %d, challenge %q", what, status, challenge)
		}
	}
	for _, p := range []string{exportStartPath, exportStatusPath, exportCancelPath} {
		r := exportCall(t, ls, readerToken, p, exportIDReq{ExportID: id})
		refused(p, r.status, r.header.Get("WWW-Authenticate"))
	}
	resp, _ := download(t, ls, readerToken, id, nil, http.MethodGet)
	refused("download", resp.StatusCode, resp.Header.Get("WWW-Authenticate"))

	for _, p := range []string{exportStatusPath, exportCancelPath} {
		if r := exportCall(t, ls, otherToken, p, exportIDReq{ExportID: id}); r.status != http.StatusNotFound || r.str("error") != msgExportNotFound {
			t.Errorf("other's %s: %d %v", p, r.status, r.body)
		}
	}
	if resp, body := download(t, ls, otherToken, id, nil, http.MethodGet); resp.StatusCode != http.StatusNotFound || errorOf(body) != msgExportNotFound {
		t.Errorf("other's download: %d %s", resp.StatusCode, body)
	}
	if n := logs.FilterMessage("export of another token").FilterField(zap.String(logFieldExportID, id)).
		FilterField(zap.String(logFieldBearer, otherLabel)).Len(); n != 3 {
		t.Errorf("%d refusals of other logged, want 3", n)
	}
	if st := exportStatusOf(t, ls, exporterToken, id); st.str("state") != exportStateDone {
		t.Errorf("after other's cancel: %v", st.body)
	}
	wantPair(t, ls, id, true)

	open, _ := exportServer(t, func(c *Config) { c.Tokens = nil }, nil)
	resp, body := do(t, newReq(t, http.MethodPost, open.url+exportStartPath, strings.NewReader("{}")))
	if resp.StatusCode != http.StatusForbidden || !strings.Contains(body, "--token-file") {
		t.Errorf("export without tokens: %d %s", resp.StatusCode, body)
	}
}

// TestExportIDs: a download names its export by the one form of id the
// server makes; any other spelling is unknown before a file name is made of
// it.
func TestExportIDs(t *testing.T) {
	t.Parallel()
	var opened atomic.Int32
	ls, _ := exportServer(t, nil, func(s *Server) {
		s.exports.open = func(name string) (io.ReadSeekCloser, error) {
			opened.Add(1)
			return os.Open(name)
		}
	})
	id, _ := exportDone(t, ls, false)
	for _, bad := range []string{
		"x", strings.ToUpper(id), strings.ReplaceAll(id, "-", ""), "{" + id + "}", "urn:uuid:" + id, id + sidecarSuffix,
	} {
		if resp, body := download(t, ls, exporterToken, url.PathEscape(bad), nil, http.MethodGet); resp.StatusCode != http.StatusNotFound {
			t.Errorf("download of %q: %d %s", bad, resp.StatusCode, body)
		}
	}
	if resp, body := download(t, ls, exporterToken, "..%2f..", nil, http.MethodGet); resp.StatusCode != http.StatusNotFound {
		t.Errorf("download of ..%%2f..: %d %s", resp.StatusCode, body)
	}
	if n := opened.Load(); n != 0 {
		t.Errorf("%d files opened for ids not the server's", n)
	}
	if resp, _ := download(t, ls, exporterToken, id, nil, http.MethodGet); resp.StatusCode != http.StatusOK || opened.Load() != 1 {
		t.Errorf("download of %s: %d, %d files opened", id, resp.StatusCode, opened.Load())
	}
}

// TestExportDownloadHTTP: HEAD, ranges and conditional requests work as
// for any file, and the digest always names the whole package.
func TestExportDownloadHTTP(t *testing.T) {
	t.Parallel()
	ls, _ := exportServer(t, nil, nil)
	id, st := exportDone(t, ls, false)
	size := st.num("size")
	etag := `"sha256-` + st.str("sha256") + `"`
	_, whole := download(t, ls, exporterToken, id, nil, http.MethodGet)
	full := sha256.Sum256(whole)
	digest := "sha-256=:" + base64.StdEncoding.EncodeToString(full[:]) + ":"

	resp, body := download(t, ls, exporterToken, id, nil, http.MethodHead)
	if resp.StatusCode != http.StatusOK || resp.ContentLength != size || len(body) != 0 || resp.Header.Get("ETag") != etag {
		t.Errorf("HEAD: %d, length %d, %d bytes, ETag %q", resp.StatusCode, resp.ContentLength, len(body), resp.Header.Get("ETag"))
	}
	resp, body = download(t, ls, exporterToken, id, http.Header{"Range": {"bytes=0-99"}}, http.MethodGet)
	if resp.StatusCode != http.StatusPartialContent || !bytes.Equal(body, whole[:100]) || resp.Header.Get("Repr-Digest") != digest ||
		resp.Header.Get("Content-Range") != fmt.Sprintf("bytes 0-99/%d", size) {
		t.Errorf("range: %d, %d bytes, Content-Range %q, Repr-Digest %q", resp.StatusCode, len(body), resp.Header.Get("Content-Range"), resp.Header.Get("Repr-Digest"))
	}
	if resp, body = download(t, ls, exporterToken, id, http.Header{"If-None-Match": {etag}}, http.MethodGet); resp.StatusCode != http.StatusNotModified || len(body) != 0 {
		t.Errorf("If-None-Match: %d, %d bytes", resp.StatusCode, len(body))
	}
	resp, body = download(t, ls, exporterToken, id, http.Header{"Range": {"bytes=0-99"}, "If-Range": {`"sha256-other"`}}, http.MethodGet)
	if resp.StatusCode != http.StatusOK || !bytes.Equal(body, whole) {
		t.Errorf("If-Range with another ETag: %d, %d bytes, want the whole package", resp.StatusCode, len(body))
	}
	if resp, _ = download(t, ls, exporterToken, id, http.Header{"If-Match": {`"sha256-other"`}}, http.MethodGet); resp.StatusCode != http.StatusPreconditionFailed {
		t.Errorf("If-Match with another ETag: %d", resp.StatusCode)
	}
}

// TestExportDownloadLimit: MaxDownloads GETs run at once; one more is told
// when to retry, a HEAD is not counted, and a slot freed is taken again.
func TestExportDownloadLimit(t *testing.T) {
	t.Parallel()
	g := newGatedOpen(t)
	ls, _ := exportServer(t, nil, func(s *Server) { s.exports.open = g.open })
	id, _ := exportDone(t, ls, false)
	limit := ls.exports.lim.MaxDownloads
	g.hold.Store(true)
	var held []<-chan int
	for range limit {
		held = append(held, sendAsync(downloadReq(t, ls, exporterToken, id, http.MethodGet)))
	}
	for range limit {
		await(t, "a download to read", g.reading)
	}
	resp, body := download(t, ls, exporterToken, id, nil, http.MethodGet)
	if resp.StatusCode != http.StatusTooManyRequests || resp.Header.Get("Retry-After") != "30" ||
		errorOf(body) != fmt.Sprintf(msgDownloadsBusy, limit) {
		t.Errorf("a download over the limit: %d %s, Retry-After %q", resp.StatusCode, body, resp.Header.Get("Retry-After"))
	}
	if resp, _ := download(t, ls, exporterToken, id, nil, http.MethodHead); resp.StatusCode != http.StatusOK {
		t.Errorf("HEAD at the limit: %d", resp.StatusCode)
	}
	g.hold.Store(false)
	g.release()
	for _, ch := range held {
		if status := await(t, "a held download", ch); status != http.StatusOK {
			t.Errorf("a held download: %d", status)
		}
	}
	until(t, "the held downloads to end", func() bool { return downloadsOf(ls, id) == 0 && len(ls.exports.slots) == 0 })
	if resp, _ := download(t, ls, exporterToken, id, nil, http.MethodGet); resp.StatusCode != http.StatusOK {
		t.Errorf("a download once the others ended: %d", resp.StatusCode)
	}
}

// TestExportDownloadCutAtStop: a download whose client reads nothing does
// not hold a stop: it is cut when its request ends, through its writer,
// and the stop is clean.
func TestExportDownloadCutAtStop(t *testing.T) {
	t.Parallel()
	var endless atomic.Bool
	var served atomic.Int64
	ls, _ := exportServer(t, func(c *Config) { c.Limits.Export.DownloadIdle = time.Minute }, func(s *Server) {
		s.exports.open = func(name string) (io.ReadSeekCloser, error) {
			if endless.Load() {
				return &endlessFile{served: &served}, nil
			}
			return os.Open(name)
		}
	})
	id, _ := exportDone(t, ls, false)
	endless.Store(true)
	tr := &http.Transport{}
	t.Cleanup(tr.CloseIdleConnections)
	answered := make(chan *http.Response, 1)
	req := downloadReq(t, ls, exporterToken, id, http.MethodGet)
	go func() {
		resp, err := (&http.Client{Transport: tr}).Do(req)
		if err != nil {
			answered <- nil
			return
		}
		answered <- resp
	}()
	until(t, "the download to begin", func() bool { return served.Load() > 0 })
	began := time.Now()
	if err := ls.stop(t); err != nil {
		t.Fatalf("stop during a download: %v, want a clean stop", err)
	}
	if took := time.Since(began); took > stopBound {
		t.Errorf("stop took %v", took)
	}
	if resp := await(t, "the download's answer", answered); resp != nil {
		_ = resp.Body.Close()
	}
	wantReopens(t, ls.root)
}

// TestExportDownloadResetOverH2C: downloads over cleartext HTTP/2 whose
// client resets the stream, which may cut a response just as its handler
// returns, leave the server serving and its stop clean.
func TestExportDownloadResetOverH2C(t *testing.T) {
	t.Parallel()
	var endless atomic.Bool
	var served atomic.Int64
	ls, _ := exportServer(t, func(c *Config) { c.Limits.Export.DownloadIdle = time.Minute }, func(s *Server) {
		s.exports.open = func(name string) (io.ReadSeekCloser, error) {
			if endless.Load() {
				return &endlessFile{served: &served}, nil
			}
			return os.Open(name)
		}
	})
	id, _ := exportDone(t, ls, false)
	endless.Store(true)
	var p http.Protocols
	p.SetUnencryptedHTTP2(true)
	tr := &http.Transport{Protocols: &p}
	t.Cleanup(tr.CloseIdleConnections)
	client := &http.Client{Transport: tr}
	for range h2Resets {
		before := served.Load()
		ctx, cancel := context.WithCancel(t.Context())
		resp, err := client.Do(downloadReq(t, ls, exporterToken, id, http.MethodGet).WithContext(ctx))
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		if resp.ProtoMajor != 2 {
			cancel()
			_ = resp.Body.Close()
			t.Fatalf("the download was served over %s, want HTTP/2", resp.Proto)
		}
		until(t, "the download to begin", func() bool { return served.Load() > before })
		cancel()
		_ = resp.Body.Close()
		until(t, "the reset download to end", func() bool { return downloadsOf(ls, id) == 0 })
	}
	if r := exportStatusOf(t, ls, exporterToken, id); r.status != http.StatusOK {
		t.Fatalf("status after the resets: %d %v", r.status, r.body)
	}
	if err := ls.stop(t); err != nil {
		t.Fatalf("stop after the resets: %v, want a clean stop", err)
	}
	wantReopens(t, ls.root)
}

// TestExportPrecheckNoRoom: a start the export folder has no room for is
// refused before the database is paused, and the next start runs.
func TestExportPrecheckNoRoom(t *testing.T) {
	t.Parallel()
	var low atomic.Bool
	low.Store(true)
	ls, logs := exportServer(t, nil, func(s *Server) {
		s.exports.checkSpace = func(path string, need uint64) error {
			if low.CompareAndSwap(true, false) {
				return &diskspace.LowSpaceError{Path: path, Free: 1, Need: 2}
			}
			return diskspace.Check(path, need)
		}
	})
	r := startExport(t, ls, exporterToken, false)
	if r.status != http.StatusInsufficientStorage || r.str("error") != fmt.Sprintf(msgExportNoRoom, 1, 2+diskspace.Reserve) {
		t.Fatalf("start without room: %d %v", r.status, r.body)
	}
	if n := logs.FilterMessage("server pause started").Len(); n != 0 {
		t.Errorf("%d pauses for a refused export", n)
	}
	wantNoExportFiles(t, ls)
	exportDone(t, ls, false)
}

// TestExportSizingBudget: sizing that outlasts its budget is given up, and
// the export starts without the early check.
func TestExportSizingBudget(t *testing.T) {
	t.Parallel()
	ls, logs := exportServer(t, nil, func(s *Server) {
		s.exports.estimate = func(ctx context.Context, root string, withoutDerived bool) (uint64, error) {
			<-ctx.Done()
			return 0, ctx.Err()
		}
	})
	began := time.Now()
	r := startExport(t, ls, exporterToken, false)
	id := r.str("export_id")
	if r.status != http.StatusAccepted || id == "" {
		t.Fatalf("start: %d %v", r.status, r.body)
	}
	if took, budget := time.Since(began), ls.exports.lim.SizingBudget; took < budget {
		t.Errorf("start answered after %v, before the sizing budget %v ran out", took, budget)
	}
	if _, ok := r.body["bytes_expected"]; ok {
		t.Errorf("bytes_expected given without sizing: %v", r.body)
	}
	if n := logs.FilterMessage("export sizing skipped").FilterField(zap.String(logFieldExportID, id)).Len(); n != 1 {
		t.Errorf("%d sizing skipped records, want 1", n)
	}
	awaitExport(t, ls, exporterToken, id, exportStateDone)
	exportIdle(t, ls)
}

// TestExportStartAbandoned: a start whose request ends while the root is
// sized is no sizing failure: it is refused without an error record, and
// the next start runs.
func TestExportStartAbandoned(t *testing.T) {
	t.Parallel()
	var hold atomic.Bool
	hold.Store(true)
	ls, logs := exportServer(t, nil, func(s *Server) {
		s.exports.estimate = func(ctx context.Context, root string, withoutDerived bool) (uint64, error) {
			if hold.CompareAndSwap(true, false) {
				<-ctx.Done()
				return 0, ctx.Err()
			}
			return dbpackage.EstimateNeed(ctx, root, withoutDerived)
		}
	})
	ctx, cancel := context.WithCancel(asExporter(t))
	cancel()
	if _, uerr := ls.exports.start(ctx, false); uerr == nil || uerr.status != http.StatusServiceUnavailable || uerr.msg != msgExportAbandoned {
		t.Fatalf("a start whose request ended: %+v", uerr)
	}
	if n := logs.FilterMessage("export sizing failed").Len(); n != 0 {
		t.Errorf("%d sizing failures logged for a request that ended", n)
	}
	wantNoExportFiles(t, ls)
	exportDone(t, ls, false)
}

// TestExportCancelWhilePausing: an export cancelled while the uploads being
// stored drain gives the pause up; the upload is stored once it may go on,
// and the folder holds nothing.
func TestExportCancelWhilePausing(t *testing.T) {
	t.Parallel()
	storing, proceed := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	ls, logs := exportServer(t, func(c *Config) {
		c.Tokens = nil
		c.Limits.Upload.FinalizeWait = finalizeWait
		c.Exports.Drain = queueWait
	}, func(s *Server) {
		next := s.uploads.runIngest
		s.uploads.runIngest = func(ctx context.Context, req ingest.Request) (ingest.Result, error) {
			if calls.Add(1) == 1 {
				close(storing)
			}
			<-proceed
			return next(ctx, req)
		}
	})
	unblock := sync.OnceFunc(func() { close(proceed) })
	t.Cleanup(unblock)
	data := []byte("stored while an export waits for it")
	id := receive(t, ls, "/case/held.bin", plainHash(ls.db(), data), data)
	answers := make(chan reply, 1)
	finalizeAt(ls, id, answers)
	await(t, "the ingest", storing)

	ex := ls.exports
	j, uerr := ex.start(asExporter(t), false)
	if uerr != nil {
		t.Fatalf("start: %v", uerr)
	}
	until(t, "the queue to pause", func() bool { return queuePaused(ls.uploads.queue) })
	st, code, uerr := ex.cancelJob(j)
	if uerr != nil || code != http.StatusAccepted || st.State != exportStatePausing {
		t.Fatalf("cancel while pausing: %d %+v %v", code, st, uerr)
	}
	until(t, "the export to be cancelled", func() bool { return ex.status(j).State == exportStateCancelled })
	exportIdle(t, ls)
	if n := logs.FilterMessage("server pause given up").FilterField(zap.String("stage", stageUploads)).
		FilterField(zap.String(logFieldPauseFor, exportPauseFor+j.id)).Len(); n != 1 {
		t.Errorf("%d pauses given up at %s, want 1", n, stageUploads)
	}
	if n := logs.FilterMessage("export cancelled").FilterField(zap.String("stage", exportStatePausing)).Len(); n != 1 {
		t.Errorf("%d cancels while pausing logged, want 1", n)
	}
	unblock()
	wantStored(t, ls, id)
	await(t, "the finalize answer", answers)
	wantNoExportFiles(t, ls)
}

// TestExportCancelWhilePackaging: an export cancelled while it writes
// leaves no file, and the database resumes once.
func TestExportCancelWhilePackaging(t *testing.T) {
	t.Parallel()
	h := newHoldExport(t)
	var resumes func() []dbopen.ResumeOptions
	ls, logs := exportServer(t, nil, func(s *Server) {
		s.exports.export = h.export
		resumes = recordResumes(s, nil)
	})
	r := startExport(t, ls, exporterToken, false)
	id := r.str("export_id")
	h.wait(t)
	c := cancelExport(t, ls, exporterToken, id)
	if c.status != http.StatusAccepted || c.str("state") != exportStatePackaging {
		t.Fatalf("cancel while packaging: %d %v", c.status, c.body)
	}
	st := awaitExport(t, ls, exporterToken, id, exportStateCancelled, exportStateFailed, exportStateDone)
	exportIdle(t, ls)
	if st.str("state") != exportStateCancelled || st.str("error") != "" || st.str("expires_at") == "" {
		t.Errorf("after the cancel: %v", st.body)
	}
	if got := resumes(); len(got) != 1 {
		t.Errorf("%d resumes, want 1", len(got))
	}
	if n := logs.FilterMessage("export cancelled").FilterField(zap.String("stage", exportStatePackaging)).
		FilterField(zap.String(logFieldBearer, exporterLabel)).Len(); n != 1 {
		t.Errorf("%d cancels while packaging logged, want 1", n)
	}
	wantNoExportFiles(t, ls)
	if c := cancelExport(t, ls, exporterToken, id); c.status != http.StatusOK || c.str("state") != exportStateCancelled {
		t.Errorf("a second cancel: %d %v", c.status, c.body)
	}
}

// TestExportCancelPublishRace: a cancel that races the publish lands wholly
// before it, leaving no file, or after it, removing the package; it never
// leaves a package behind once answered.
func TestExportCancelPublishRace(t *testing.T) {
	t.Parallel()
	const rounds = 20
	var inRename atomic.Bool
	exporting := make(chan string, 1)
	renamed := make(chan (<-chan reply), 1)
	ls, _ := exportServer(t, nil, func(s *Server) {
		ex := s.exports
		ex.export = func(ctx context.Context, opts dbpackage.ExportOptions) (dbpackage.ExportResult, error) {
			if !inRename.Load() {
				exporting <- strings.TrimSuffix(filepath.Base(opts.Output), dbpackage.Ext+partialSuffix)
			}
			return dbpackage.Export(ctx, opts)
		}
		ex.rename = func(src, dst string, overwrite bool) error {
			if inRename.Load() {
				id := strings.TrimSuffix(filepath.Base(dst), dbpackage.Ext)
				// The listeners were opened before the server's goroutines began.
				renamed <- callAsync("http://"+s.Addrs()[0].String(), exporterToken, exportCancelPath, exportIDReq{ExportID: id})
			}
			return fsx.ReplaceFile(src, dst, overwrite)
		}
	})
	for i := range rounds {
		inRename.Store(i%2 == 1)
		r := startExport(t, ls, exporterToken, false)
		id := r.str("export_id")
		if r.status != http.StatusAccepted {
			t.Fatalf("round %d: start: %d %v", i, r.status, r.body)
		}
		var answer <-chan reply
		if inRename.Load() {
			answer = await(t, "the rename", renamed)
		} else {
			if got := await(t, "the export to package", exporting); got != id {
				t.Fatalf("round %d: export %s packaged, want %s", i, got, id)
			}
			answer = callAsync(ls.url, exporterToken, exportCancelPath, exportIDReq{ExportID: id})
		}
		c := await(t, "the cancel's answer", answer)
		if c.status != http.StatusOK && c.status != http.StatusAccepted {
			t.Fatalf("round %d: cancel: %d %v", i, c.status, c.body)
		}
		awaitExport(t, ls, exporterToken, id, exportStateCancelled, exportStateDone, exportStateFailed)
		exportIdle(t, ls)
		if st := exportStatusOf(t, ls, exporterToken, id); st.str("state") != exportStateCancelled {
			t.Fatalf("round %d: cancel answered %d, but the export ended %v", i, c.status, st.body)
		}
		wantPair(t, ls, id, false)
		wantNoExportFiles(t, ls)
	}
}

// TestExportPublishOrder: the sidecar is written, with the package's
// SHA-256, before the package gets its name.
func TestExportPublishOrder(t *testing.T) {
	t.Parallel()
	var checked atomic.Int32
	ls, _ := exportServer(t, nil, func(s *Server) {
		ex := s.exports
		ex.rename = func(src, dst string, overwrite bool) error {
			checked.Add(1)
			id := strings.TrimSuffix(filepath.Base(dst), dbpackage.Ext)
			if overwrite {
				t.Error("the publish may replace a package")
			}
			if _, err := os.Stat(dst); !errors.Is(err, fs.ErrNotExist) {
				t.Errorf("the package is named before it is published (%v)", err)
			}
			sc, err := readSidecar(ex.file(id, sidecarSuffix))
			if err != nil {
				t.Errorf("no sidecar before the package is named: %v", err)
			}
			data, err := os.ReadFile(src)
			if err != nil {
				t.Errorf("the written package: %v", err)
			}
			if sum := sha256.Sum256(data); hex.EncodeToString(sum[:]) != sc.SHA256 || int64(len(data)) != sc.Size {
				t.Errorf("the sidecar says %d bytes of sha256 %s; the package holds %d of %x", sc.Size, sc.SHA256, len(data), sum)
			}
			return fsx.ReplaceFile(src, dst, overwrite)
		}
	})
	id, st := exportDone(t, ls, false)
	if n := checked.Load(); n != 1 {
		t.Fatalf("%d renames, want 1", n)
	}
	sc, err := readSidecar(ls.exports.file(id, sidecarSuffix))
	if err != nil || sc.SHA256 != st.str("sha256") || sc.Owner != exporterLabel || sc.ExportID != id {
		t.Errorf("sidecar %+v (%v), status %v", sc, err, st.body)
	}
}

// TestExportFailures: how an export fails is told by its status, in words
// that name no server path; the detail goes to the log, and nothing of the
// export is left.
func TestExportFailures(t *testing.T) {
	t.Parallel()
	const secret = "the disk at /srv/secret/place broke"
	cases := []struct {
		err       error
		msg       string
		retryable bool
	}{
		{fmt.Errorf("pause: %w", errPausing), msgExportPausing, true},
		{fmt.Errorf("pause: %w", errPauseStuck), msgExportBusy, true},
		{fmt.Errorf("pause: %w", errStopping), msgExportStopped, true},
		{fmt.Errorf("%w: test", dbopen.ErrNeedsRecovery), msgExportUnclean, true},
		{&diskspace.LowSpaceError{Path: "x", Free: 1, Need: 2}, msgExportNoSpace, false},
		{errors.New(secret), "", false},
	}
	var calls atomic.Int32
	ls, logs := exportServer(t, nil, func(s *Server) {
		s.exports.export = func(context.Context, dbpackage.ExportOptions) (dbpackage.ExportResult, error) {
			return dbpackage.ExportResult{}, cases[calls.Add(1)-1].err
		}
	})
	for _, c := range cases {
		r := startExport(t, ls, exporterToken, false)
		id := r.str("export_id")
		if r.status != http.StatusAccepted {
			t.Fatalf("start: %d %v", r.status, r.body)
		}
		st := awaitExport(t, ls, exporterToken, id, exportStateFailed, exportStateDone, exportStateCancelled)
		exportIdle(t, ls)
		want := c.msg
		if want == "" {
			want = fmt.Sprintf(msgExportInternal, id)
		}
		retryable, _ := st.body["retryable"].(bool)
		if st.str("state") != exportStateFailed || st.str("error") != want || retryable != c.retryable {
			t.Errorf("%v: status %v, want failed with %q, retryable %v", c.err, st.body, want, c.retryable)
		}
		failed := logs.FilterMessage("export failed").FilterField(zap.String(logFieldExportID, id)).All()
		if len(failed) != 1 || !strings.Contains(fmt.Sprint(failed[0].ContextMap()["error"]), c.err.Error()) {
			t.Errorf("%v: export failed records %v", c.err, failed)
		}
		wantNoExportFiles(t, ls)
	}
	ls.exports.mu.Lock()
	defer ls.exports.mu.Unlock()
	for _, j := range ls.exports.jobs {
		if strings.Contains(j.errMsg, secret) {
			t.Errorf("a status names the failure's detail: %q", j.errMsg)
		}
	}
}

// TestExportCancelDone: a cancel removes a written package, unless it is
// being downloaded; the cancelled export is forgotten after DoneTTL.
func TestExportCancelDone(t *testing.T) {
	t.Parallel()
	clock := &fakeClock{t: time.Now()}
	g := newGatedOpen(t)
	ls, logs := exportServer(t, nil, func(s *Server) {
		s.exports.now = clock.Now
		s.exports.open = g.open
	})
	ex := ls.exports
	id, _ := exportDone(t, ls, false)
	c := cancelExport(t, ls, exporterToken, id)
	if c.status != http.StatusOK || c.str("state") != exportStateCancelled || c.str("download") != "" {
		t.Fatalf("cancel of a written export: %d %v", c.status, c.body)
	}
	wantPair(t, ls, id, false)
	if n := logs.FilterMessage("export removed").FilterField(zap.String(logFieldExportID, id)).FilterField(zap.String("reason", removedCancelled)).Len(); n != 1 {
		t.Errorf("%d removals logged, want 1", n)
	}
	if resp, body := download(t, ls, exporterToken, id, nil, http.MethodGet); resp.StatusCode != http.StatusConflict ||
		errorOf(body) != fmt.Sprintf(msgExportNotDone, exportStateCancelled) {
		t.Errorf("download of a cancelled export: %d %s", resp.StatusCode, body)
	}
	clock.Add(ex.lim.DoneTTL + time.Second)
	ex.reap()
	if st := exportStatusOf(t, ls, exporterToken, id); st.status != http.StatusNotFound {
		t.Errorf("status once forgotten: %d %v", st.status, st.body)
	}
	if resp, _ := download(t, ls, exporterToken, id, nil, http.MethodGet); resp.StatusCode != http.StatusNotFound {
		t.Errorf("download once forgotten: %d", resp.StatusCode)
	}

	held, _ := exportDone(t, ls, false)
	g.hold.Store(true)
	dl := sendAsync(downloadReq(t, ls, exporterToken, held, http.MethodGet))
	await(t, "the download to read", g.reading)
	if c := cancelExport(t, ls, exporterToken, held); c.status != http.StatusConflict || c.str("error") != msgExportDownloading {
		t.Errorf("cancel during a download: %d %v", c.status, c.body)
	}
	wantPair(t, ls, held, true)
	g.hold.Store(false)
	g.release()
	if status := await(t, "the download", dl); status != http.StatusOK {
		t.Errorf("the download: %d", status)
	}
	until(t, "the download to end", func() bool { return downloadsOf(ls, held) == 0 })
	if c := cancelExport(t, ls, exporterToken, held); c.status != http.StatusOK || c.str("state") != exportStateCancelled {
		t.Errorf("cancel after the download: %d %v", c.status, c.body)
	}
	wantPair(t, ls, held, false)
}

// TestExportRetention: a package is removed once it expires, unless it is
// being downloaded; then it goes at the first round after.
func TestExportRetention(t *testing.T) {
	t.Parallel()
	clock := &fakeClock{t: time.Now()}
	g := newGatedOpen(t)
	ls, logs := exportServer(t, nil, func(s *Server) {
		s.exports.now = clock.Now
		s.exports.open = g.open
	})
	ex := ls.exports
	id, _ := exportDone(t, ls, false)
	clock.Add(ex.cfg.Keep - time.Minute)
	ex.reap()
	if st := exportStatusOf(t, ls, exporterToken, id); st.str("state") != exportStateDone {
		t.Fatalf("before it expires: %d %v", st.status, st.body)
	}
	wantPair(t, ls, id, true)
	clock.Add(2 * time.Minute)
	ex.reap()
	if st := exportStatusOf(t, ls, exporterToken, id); st.status != http.StatusNotFound {
		t.Errorf("once expired: %d %v", st.status, st.body)
	}
	wantPair(t, ls, id, false)
	if n := logs.FilterMessage("export removed").FilterField(zap.String(logFieldExportID, id)).FilterField(zap.String("reason", removedExpired)).Len(); n != 1 {
		t.Errorf("%d expiries logged, want 1", n)
	}

	held, _ := exportDone(t, ls, false)
	g.hold.Store(true)
	dl := sendAsync(downloadReq(t, ls, exporterToken, held, http.MethodGet))
	await(t, "the download to read", g.reading)
	clock.Add(ex.cfg.Keep + time.Minute)
	ex.reap()
	if st := exportStatusOf(t, ls, exporterToken, held); st.str("state") != exportStateDone {
		t.Errorf("expired while downloaded: %d %v", st.status, st.body)
	}
	wantPair(t, ls, held, true)
	g.hold.Store(false)
	g.release()
	if status := await(t, "the download", dl); status != http.StatusOK {
		t.Errorf("the download: %d", status)
	}
	until(t, "the download to end", func() bool { return downloadsOf(ls, held) == 0 })
	ex.reap()
	if st := exportStatusOf(t, ls, exporterToken, held); st.status != http.StatusNotFound {
		t.Errorf("after the download: %d %v", st.status, st.body)
	}
	wantPair(t, ls, held, false)
}

// TestExportMaxKept: no more than MaxKept packages are kept; a cancel makes
// room.
func TestExportMaxKept(t *testing.T) {
	t.Parallel()
	ls, _ := exportServer(t, func(c *Config) { c.Limits.Export.MaxKept = 1 }, nil)
	id, _ := exportDone(t, ls, false)
	if r := startExport(t, ls, exporterToken, false); r.status != http.StatusConflict || r.str("error") != fmt.Sprintf(msgExportsKept, 1) {
		t.Errorf("a start with the most kept: %d %v", r.status, r.body)
	}
	if c := cancelExport(t, ls, exporterToken, id); c.status != http.StatusOK {
		t.Fatalf("cancel: %d %v", c.status, c.body)
	}
	exportDone(t, ls, false)
}

// TestExportStartupSweep: a new server removes unfinished exports and
// halves of pairs, removes expired and broken pairs, serves this database's
// finished ones to their owner, and leaves another database's and every
// other name alone.
func TestExportStartupSweep(t *testing.T) {
	t.Parallel()
	h := openDB(t)
	dir := filepath.Join(h.Root, cnst.ExportsDir)
	if err := os.MkdirAll(dir, cnst.DirPerm); err != nil {
		t.Fatal(err)
	}
	write := func(name string, data []byte) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now()
	pair := func(content string, edit func(*exportSidecar)) string {
		t.Helper()
		id := uuid.NewString()
		sum := sha256.Sum256([]byte(content))
		sc := exportSidecar{
			Version: sidecarVersion, ExportID: id, Owner: exporterLabel, DatabaseRoot: h.Root, DatabaseCreated: h.Manifest.Created,
			CreatedAt: now.Add(-time.Hour).UTC(), DoneAt: now.Add(-time.Hour).UTC(), ExpiresAt: now.Add(time.Hour).UTC(),
			Size: int64(len(content)), SHA256: hex.EncodeToString(sum[:]), ContentSHA256: "content",
		}
		if edit != nil {
			edit(&sc)
		}
		data, err := json.Marshal(sc)
		if err != nil {
			t.Fatal(err)
		}
		write(id+dbpackage.Ext, []byte(content))
		write(id+sidecarSuffix, data)
		return id
	}
	temp := func(target, suffix string) string { return strings.Replace(fsx.TempPattern(target), "*", suffix, 1) }

	const liveContent = "a package of an earlier run"
	live := pair(liveContent, nil)
	expired := pair("expired", func(sc *exportSidecar) { sc.ExpiresAt = now.Add(-time.Minute).UTC() })
	foreign := pair("another database's", func(sc *exportSidecar) { sc.DatabaseCreated = h.Manifest.Created.Add(time.Second) })
	broken := pair("size does not match", func(sc *exportSidecar) { sc.Size++ })
	partial, tempPkg, tempSide, loneSide, lonePkg := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	write(partial+dbpackage.Ext+partialSuffix, []byte("unfinished"))
	write(temp(tempPkg+dbpackage.Ext+partialSuffix, "123"), []byte("killed"))
	write(temp(tempSide+sidecarSuffix, "456"), []byte("killed"))
	write(loneSide+sidecarSuffix, []byte("{}"))
	write(lonePkg+dbpackage.Ext, []byte("orphan"))
	upper := strings.ToUpper(uuid.NewString())
	kept := []string{"notes.txt", "x" + dbpackage.Ext, upper + dbpackage.Ext, upper + sidecarSuffix, exportsLockName, temp("notes.txt", "1")}
	for _, n := range kept {
		write(n, []byte("not an export's"))
	}
	folder := uuid.NewString() + dbpackage.Ext
	if err := os.Mkdir(filepath.Join(dir, folder), cnst.DirPerm); err != nil {
		t.Fatal(err)
	}

	ls, logs := exportServerOn(t, h, nil, nil)
	want := append(slices.Clone(kept), folder, live+dbpackage.Ext, live+sidecarSuffix, foreign+dbpackage.Ext, foreign+sidecarSuffix)
	slices.Sort(want)
	if got := exportFiles(t, ls); !slices.Equal(got, want) {
		t.Errorf("the folder holds %v, want %v", got, want)
	}
	for _, n := range []string{
		"removed 6 unfinished or orphaned exports left by an earlier run",
		"kept 1 exports of an earlier run until they expire",
		fmt.Sprintf("left 1 exports of another database in %s", dir),
	} {
		if !slices.Contains(ls.Notes, n) {
			t.Errorf("notes %q, want %q", ls.Notes, n)
		}
	}
	for id, reason := range map[string]string{expired: removedExpired, broken: removedInvalid, partial: removedLeftOver, lonePkg: removedOrphan} {
		if n := logs.FilterMessage("export removed").FilterField(zap.String(logFieldExportID, id)).FilterField(zap.String("reason", reason)).Len(); n != 1 {
			t.Errorf("%d removals of %s logged as %s, want 1", n, id, reason)
		}
	}

	st := exportStatusOf(t, ls, exporterToken, live)
	sum := sha256.Sum256([]byte(liveContent))
	if st.status != http.StatusOK || st.str("state") != exportStateDone || st.str("sha256") != hex.EncodeToString(sum[:]) {
		t.Fatalf("the adopted export: %d %v", st.status, st.body)
	}
	if resp, body := download(t, ls, exporterToken, live, nil, http.MethodGet); resp.StatusCode != http.StatusOK || string(body) != liveContent {
		t.Errorf("download of the adopted export: %d %q", resp.StatusCode, body)
	}
	if st := exportStatusOf(t, ls, otherToken, live); st.status != http.StatusNotFound {
		t.Errorf("another token's status of the adopted export: %d", st.status)
	}
	if st := exportStatusOf(t, ls, exporterToken, foreign); st.status != http.StatusNotFound {
		t.Errorf("another database's export: %d %v", st.status, st.body)
	}
}

// TestExportDirChecks: the export folder is the root's exports folder, one
// in it, or a folder outside the root that one server holds; a link from
// outside into the root is refused.
func TestExportDirChecks(t *testing.T) {
	t.Parallel()
	h := openDB(t)
	newWith := func(h *dbopen.Handle, dir string) (*Server, error) {
		return New(Config{Limits: testLimits(), Exports: ExportConfig{Dir: dir}}, Deps{Handle: h})
	}
	elsewhere := filepath.Join(h.Root, "elsewhere")
	for _, dir := range []string{elsewhere, filepath.Join(h.Root, "Exports")} {
		if _, err := newWith(h, dir); err == nil || !strings.Contains(err.Error(), "inside the database") {
			t.Errorf("export folder %s: %v", dir, err)
		}
	}
	if _, err := os.Stat(elsewhere); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a refused folder was made (%v)", err)
	}
	sub := filepath.Join(h.Root, cnst.ExportsDir, "sub")
	if s, err := newWith(h, sub); err != nil || s.exports.dir != sub || !s.exports.inRoot || s.exports.lock != nil {
		t.Errorf("export folder %s: %v", sub, err)
	}

	if link := filepath.Join(t.TempDir(), "into-root"); os.Symlink(h.Root, link) == nil {
		if _, err := newWith(h, link); err == nil || !strings.Contains(err.Error(), "inside the database") {
			t.Errorf("a link into the root: %v", err)
		}
	} else {
		t.Log("this system makes no symbolic link; the link case is not checked")
	}

	// Another database's exports folder is refused: that database's server
	// sweeps it without the folder lock.
	if _, err := newWith(openDB(t), filepath.Join(h.Root, cnst.ExportsDir)); err == nil || !strings.Contains(err.Error(), "inside the database") {
		t.Errorf("another database's exports folder: %v", err)
	}

	out := t.TempDir()
	ls := startServer(t, Config{Exports: ExportConfig{Dir: out}}, Deps{Handle: h}, nil)
	if _, err := os.Stat(filepath.Join(out, exportsLockName)); err != nil || ls.exports.inRoot {
		t.Errorf("an outside folder is not locked (%v), in root %v", err, ls.exports.inRoot)
	}
	second := openDB(t)
	if _, err := newWith(second, out); err == nil || !strings.Contains(err.Error(), "in use by another DUES server") {
		t.Errorf("a second server on the folder: %v", err)
	}
	if err := ls.stop(t); err != nil {
		t.Fatalf("stop: %v", err)
	}
	s, err := newWith(second, out)
	if err != nil {
		t.Fatalf("a server on the folder once the first stopped: %v", err)
	}
	t.Cleanup(func() {
		if err := s.exports.lock.Unlock(); err != nil {
			t.Error(err)
		}
	})
}

// TestExportUploadsReserve: while an export into the data root packages,
// an upload must leave free what the export has still to write.
func TestExportUploadsReserve(t *testing.T) {
	t.Parallel()
	const (
		uploadBytes = 1 << 10
		expected    = 1 << 30
		slack       = 1 << 10
	)
	h := newHoldExport(t)
	ls, _ := exportServer(t, func(c *Config) { c.Tokens = nil }, func(s *Server) {
		ex := s.exports
		ex.estimate = func(context.Context, string, bool) (uint64, error) { return expected, nil }
		ex.checkSpace = func(string, uint64) error { return nil }
		ex.export = h.export
		headroom := uint64(s.uploads.lim.Headroom)
		s.uploads.free = func(string) (uint64, error) { return uploadBytes + headroom + slack, nil }
	})
	data := bytes.Repeat([]byte("u"), uploadBytes)
	hash := plainHash(ls.db(), data)
	j, uerr := ls.exports.start(asExporter(t), false)
	if uerr != nil {
		t.Fatalf("start: %v", uerr)
	}
	h.wait(t)
	if r := start(t, ls, "/case/during.bin", "", hash, uploadBytes); r.status != http.StatusInsufficientStorage ||
		!strings.Contains(r.str("error"), "bytes are free for uploads") {
		t.Errorf("an upload while the export packages: %d %v", r.status, r.body)
	}
	h.release()
	until(t, "the export to be written", func() bool { return ls.exports.status(j).State == exportStateDone })
	exportIdle(t, ls)
	r := start(t, ls, "/case/after.bin", "", hash, uploadBytes)
	if r.status != http.StatusOK {
		t.Fatalf("an upload after the export: %d %v", r.status, r.body)
	}
	if a := postJSON(t, ls, "/web/upload/abort", map[string]string{"upload_id": r.str("upload_id")}); a.status != http.StatusOK {
		t.Errorf("abort: %d %v", a.status, a.body)
	}
}

// TestExportAuditLog: the log ties an export's start, package, pause,
// download and removal to its token's label, and holds no token, nor does
// the sidecar.
func TestExportAuditLog(t *testing.T) {
	t.Parallel()
	ls, logs := exportServer(t, nil, nil)
	r := startExport(t, ls, exporterToken, false)
	id := r.str("export_id")
	awaitExport(t, ls, exporterToken, id, exportStateDone)
	exportIdle(t, ls)
	resp, _ := download(t, ls, exporterToken, id, nil, http.MethodGet)
	// The client has the whole body before the handler logs it and ends.
	until(t, "the download to end", func() bool { return downloadsOf(ls, id) == 0 })
	side, err := os.ReadFile(ls.exports.file(id, sidecarSuffix))
	if err != nil {
		t.Fatal(err)
	}
	if c := cancelExport(t, ls, exporterToken, id); c.status != http.StatusOK {
		t.Fatalf("cancel: %d %v", c.status, c.body)
	}
	one := func(msg string) map[string]any {
		t.Helper()
		e := logs.FilterMessage(msg).FilterField(zap.String(logFieldExportID, id)).All()
		if len(e) != 1 {
			t.Fatalf("%d %q records, want 1", len(e), msg)
		}
		return e[0].ContextMap()
	}
	for _, msg := range []string{"export started", "export written", "export downloaded"} {
		if got := one(msg)[logFieldBearer]; got != exporterLabel {
			t.Errorf("%s: bearer %v, want %s", msg, got, exporterLabel)
		}
	}
	if got := one("export started")["request_id"]; got != r.header.Get("Request-Id") {
		t.Errorf("export started: request %v, want %s", got, r.header.Get("Request-Id"))
	}
	if e := one("export downloaded"); e["request_id"] != resp.Header.Get("Request-Id") || e["range"] != false {
		t.Errorf("export downloaded: %v", e)
	}
	if got := one("export removed")["reason"]; got != removedCancelled {
		t.Errorf("export removed: reason %v", got)
	}
	for _, msg := range []string{"server pause started", "server paused", "server resumed"} {
		if n := logs.FilterMessage(msg).FilterField(zap.String(logFieldPauseFor, exportPauseFor+id)).Len(); n != 1 {
			t.Errorf("%d %q records for the export, want 1", n, msg)
		}
	}
	for _, tok := range []string{exporterToken, otherToken, readerToken} {
		for _, e := range logs.All() {
			if strings.Contains(fmt.Sprint(e.ContextMap()), tok) || strings.Contains(e.Message, tok) {
				t.Fatalf("a token reached the log: %s %v", e.Message, e.ContextMap())
			}
		}
		if bytes.Contains(side, []byte(tok)) {
			t.Fatalf("a token reached the sidecar: %s", side)
		}
	}
	var sc exportSidecar
	if err := json.Unmarshal(side, &sc); err != nil || sc.Owner != exporterLabel {
		t.Errorf("sidecar owner %q (%v)", sc.Owner, err)
	}
}

// TestExportStopMidExport: a stop while an export writes ends it, leaves no
// file, and is clean; the database reopens.
func TestExportStopMidExport(t *testing.T) {
	t.Parallel()
	h := newHoldExport(t)
	ls, _ := exportServer(t, nil, func(s *Server) { s.exports.export = h.export })
	if r := startExport(t, ls, exporterToken, false); r.status != http.StatusAccepted {
		t.Fatalf("start: %d %v", r.status, r.body)
	}
	h.wait(t)
	if err := ls.stop(t); err != nil {
		t.Fatalf("stop during an export: %v, want a clean stop", err)
	}
	wantNoExportFiles(t, ls)
	wantReopens(t, ls.root)
}

// TestExportSettings: the settings' ranges, for flags, config files and
// library callers alike.
func TestExportSettings(t *testing.T) {
	t.Parallel()
	for h, ok := range map[int]bool{0: false, MinExportKeepHours: true, DefaultExportKeepHours: true, MaxExportKeepHours: true, MaxExportKeepHours + 1: false, -1: false} {
		if err := CheckExportKeepHours(h); (err == nil) != ok {
			t.Errorf("keep %d hours: %v", h, err)
		}
	}
	for s, want := range map[string]time.Duration{"1s": time.Second, "90s": 90 * time.Second, "5m0s": DefaultDrainTimeout, "1h": time.Hour, "0s": 0, "999ms": 0, "2h": 0, "soon": 0, "": 0} {
		d, err := ParseDrainTimeout(s)
		if want == 0 && err == nil || want != 0 && (err != nil || d != want) {
			t.Errorf("drain %q: %v, %v", s, d, err)
		}
	}
	abs := t.TempDir()
	for _, c := range []struct {
		name    string
		e       ExportConfig
		wantErr string
	}{
		{"zero", ExportConfig{}, ""},
		{"set", ExportConfig{Dir: abs, Keep: 48 * time.Hour, Drain: 90 * time.Second}, ""},
		{"relative folder", ExportConfig{Dir: "exports"}, "not an absolute path"},
		{"part of an hour", ExportConfig{Keep: 90 * time.Minute}, "whole number of hours"},
		{"kept too long", ExportConfig{Keep: (MaxExportKeepHours + 1) * time.Hour}, "export keep time"},
		{"drain too short", ExportConfig{Drain: time.Millisecond}, "export drain timeout"},
	} {
		err := (Config{Exports: c.e}).Validate()
		if c.wantErr == "" && err != nil || c.wantErr != "" && (err == nil || !strings.Contains(err.Error(), c.wantErr)) {
			t.Errorf("%s: %v, want %q", c.name, err, c.wantErr)
		}
	}
	root := filepath.Join(abs, "db")
	for dir, ok := range map[string]bool{
		"":                                   true,
		filepath.Join(abs, "elsewhere"):      true,
		filepath.Join(root, cnst.ExportsDir): true,
		filepath.Join(root, cnst.ExportsDir, "sub"): true,
		root:                               false,
		filepath.Join(root, cnst.BLOBSDIR): false,
		filepath.Join(root, "Exports"):     false,
		"relative":                         false,
	} {
		if err := CheckExportDir(root, dir); (err == nil) != ok {
			t.Errorf("CheckExportDir(%s): %v", dir, err)
		}
	}
}

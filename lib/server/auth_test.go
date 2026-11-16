package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"indicer/lib/util"
	"indicer/pb"
	"indicer/pb/pbconnect"

	"connectrpc.com/connect"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

func mustCredential(t testing.TB, token string, scopes Scope, label string) Credential {
	t.Helper()
	c, err := NewCredential(token, scopes, label)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// testToken40 is a token of 40 c's.
func testToken40(c byte) string { return strings.Repeat(string(c), 40) }

// testRoute registers h at pattern for tokens holding scope.
func testRoute(s *Server, pattern string, scope Scope, h http.HandlerFunc) {
	s.handle(pattern, route{scope: scope}, h)
}

func bearer(req *http.Request, token string) *http.Request {
	req.Header.Set("Authorization", "Bearer "+token)
	return req
}

func TestNewCredential(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name, token string
		scopes      Scope
		label       string
		wantErr     string
	}{
		{"ok", testToken, ScopeRead, "case-1.a_b", ""},
		{"longest label", testToken, ScopeAll, strings.Repeat("l", MaxLabelLength), ""},
		{"short token", testToken[:MinTokenLength-1], ScopeRead, "a", "at least 32"},
		{"very short token", "short", ScopeRead, "a", "at least 32"},
		{"token with space", strings.Repeat("a", 20) + " " + strings.Repeat("b", 20), ScopeRead, "a", "visible ASCII"},
		{"no scope", testToken, 0, "a", "scopes"},
		{"unknown scope", testToken, ScopeAll + 1<<5, "a", "scopes"},
		{"no label", testToken, ScopeRead, "", "label"},
		{"label as long as a token", testToken, ScopeRead, strings.Repeat("l", MinTokenLength), "label"},
		{"label with @", testToken, ScopeRead, "a@b", "label"},
	} {
		t.Run(c.name, func(t *testing.T) {
			cred, err := NewCredential(c.token, c.scopes, c.label)
			switch {
			case c.wantErr == "" && err != nil:
				t.Fatalf("NewCredential: %v", err)
			case c.wantErr != "" && (err == nil || !strings.Contains(err.Error(), c.wantErr)):
				t.Fatalf("NewCredential: %v, want an error containing %q", err, c.wantErr)
			case err != nil && strings.Contains(err.Error(), c.token):
				t.Fatalf("the error quotes the token: %v", err)
			case err == nil && (cred.Label() != c.label || cred.Scopes() != c.scopes):
				t.Fatalf("credential %q %v", cred.Label(), cred.Scopes())
			}
		})
	}
}

// "all" in a v1 file is the v1 scopes, and stays so when scopes are added.
func TestScopeAllIsV1(t *testing.T) {
	t.Parallel()
	all, err := parseScopes("all")
	if err != nil || all != ScopeRead|ScopeUpload|ScopeExport || all.String() != "read,upload,export" {
		t.Fatalf("all = %v (%v)", all, err)
	}
}

func TestParseTokens(t *testing.T) {
	t.Parallel()
	a, b, c := testToken40('a'), testToken40('b'), testToken40('c')
	type want struct {
		label  string
		scopes Scope
	}
	v1 := func(lines ...string) string { return TokenFileHeader + "\n" + strings.Join(lines, "\n") + "\n" }
	var many []string
	var full []want
	for i := range MaxTokens + 1 {
		many = append(many, fmt.Sprintf("%s%04d read t%d", a, i, i))
		full = append(full, want{fmt.Sprintf("t%d", i), ScopeRead})
	}
	b64 := a[:20] + "~+/" + a[20:] + "=="
	for _, tc := range []struct {
		name, file string
		want       []want
		wantErr    string
	}{
		{"one token", a + "\n", []want{{DefaultLabel, DefaultScopes}}, ""},
		{"one token, BOM and CRLF", "\xEF\xBB\xBF" + a + "\r\n\r\n", []want{{DefaultLabel, DefaultScopes}}, ""},
		{"one token, trailing blanks", a + " \t\n", []want{{DefaultLabel, DefaultScopes}}, ""},
		{"one token commented out", "#" + a, nil, "line 1 starts with '#'"},
		{"one token after a comment", "# old\n" + a, nil, "line 1 starts with '#'"},
		{"one token, indented #", "  #" + a, nil, "line 1 starts with '#'"},
		{"one token, a second line", a + "\nsecond\n", nil, "line 2: a file without a header holds one token"},
		{"one token, too short", "short\n", nil, "line 1: the token must be at least 32"},
		{"empty", "", nil, "holds no token"},
		{"only blank lines", " \n\t\r\n", nil, "holds no token"},
		{"blank first line", "\n" + a, nil, "line 1 is blank"},
		{"blank line before the header", "\r\n" + v1(a+" read x"), nil, "line 1 is blank"},
		{"v1", v1(a+" read alice", "# a comment", "", b+" upload,export bob", "  "+c+"  all  carol  "), []want{
			{"alice", ScopeRead}, {"bob", ScopeUpload | ScopeExport}, {"carol", ScopeAll},
		}, ""},
		{"v1, header spaced", "#dues-tokens   v1\r\n" + a + " read x\r\n", []want{{"x", ScopeRead}}, ""},
		{"v1, a token commented out", v1("#"+a+" read old", b+" read new"), []want{{"new", ScopeRead}}, ""},
		{"v1, padding", v1(a + "== read x"), []want{{"x", ScopeRead}}, ""},
		{"v1, base64 token", v1(b64 + " read x"), []want{{"x", ScopeRead}}, ""},
		{"v1, blank-ish lines and an indented comment", v1("   \t", "  # note", a+" read x"), []want{{"x", ScopeRead}}, ""},
		{"v1, exactly the most tokens", v1(many[:MaxTokens]...), full[:MaxTokens], ""},
		{"v1, header with more on its line", TokenFileHeader + " # tokens\n" + a + " read x\n", nil, "line 1: nothing may follow"},
		{"v1, only padding", v1(strings.Repeat("=", MinTokenLength) + " read x"), nil, "RFC 6750"},
		{"v1, four columns", v1(a + " read alice extra"), nil, "line 2: it has 4 columns, not 3"},
		{"v1, token in the scopes column", v1("alice " + a + " read"), nil, "line 2: unknown scope"},
		{"v1, columns swapped", v1("alice read " + a), nil, "line 2: the token must be at least 32"},
		{"v1, label and scopes swapped", v1(a + " alice read"), nil, "line 2: unknown scope"},
		{"v1, token as label", v1(a + " read " + b), nil, "line 2: the label must be"},
		{"v1, # in a token", v1(a[:20] + "#" + a[20:] + " read x"), nil, "RFC 6750"},
		{"v1, comma in a token", v1(a[:20] + "," + a[20:] + " read x"), nil, "RFC 6750"},
		{"v1, quote in a token", v1(a[:20] + `"` + a[20:] + " read x"), nil, "RFC 6750"},
		{"v1, backslash in a token", v1(a[:20] + `\` + a[20:] + " read x"), nil, "RFC 6750"},
		{"v1, = inside a token", v1(a[:20] + "=" + a[20:] + " read x"), nil, "RFC 6750"},
		{"v1, unknown scope", v1(a + " admin x"), nil, "unknown scope"},
		{"v1, empty scope", v1(a + " read, x"), nil, "unknown scope"},
		{"v1, no label", v1(a + " read"), nil, "line 2: it has 2 columns, not 3 (a line is <token> <scopes> <label>)"},
		{"v1, same token", v1(a+" read x", a+" upload y"), nil, "lines 2 and 3 hold the same token"},
		{"v1, same label but for case", v1(a+" read Alice", b+" read alice"), nil, "lines 2 and 3 have the same label"},
		{"v1, no token", v1(), nil, "holds no token"},
		{"v1, only comments", v1("# nothing", "#"+a+" read x"), nil, "holds no token"},
		{"v1, too many", v1(many...), nil, fmt.Sprintf("line %d: a file holds at most %d tokens", MaxTokens+2, MaxTokens)},
		{"other version", "#dues-tokens v2\n" + a + " read x\n", nil, "line 1: this server reads token files that start with"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			creds, err := ParseTokens([]byte(tc.file))
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("ParseTokens: %v, want an error containing %q", err, tc.wantErr)
				}
				for f := range strings.FieldsSeq(tc.file) {
					if len(f) >= MinTokenLength && strings.Contains(err.Error(), f) {
						t.Fatalf("the error quotes the file: %v", err)
					}
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseTokens: %v", err)
			}
			var got []want
			for _, c := range creds {
				got = append(got, want{c.Label(), c.Scopes()})
			}
			if fmt.Sprint(got) != fmt.Sprint(tc.want) {
				t.Fatalf("credentials %v, want %v", got, tc.want)
			}
			if err := (Config{Tokens: creds}).Validate(); err != nil {
				t.Fatalf("Validate: %v", err)
			}
		})
	}
	// The token is what the file holds, without its line's spaces.
	creds, err := ParseTokens([]byte(v1("  "+a+"\tread x", b64+" read y")))
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{cfg: Config{Tokens: creds}}
	for _, tok := range []string{a, b64} {
		if _, ok := s.authenticate("Bearer " + tok); !ok {
			t.Fatalf("the token %q of the file is not taken", tok)
		}
	}
}

// Each route holds the scope D4 gives it, and only the routes that read the
// database through the request hold a lease on it; a route added later must
// be given both here.
func TestRouteScopes(t *testing.T) {
	t.Parallel()
	h := openDB(t)
	s, err := New(Config{Limits: testLimits()}, Deps{Handle: h})
	if err != nil {
		t.Fatal(err)
	}
	type rs struct {
		scope              Scope
		public, rpc, lease bool
	}
	rpc := func(p string) string { return "POST " + p }
	want := map[string]rs{
		"GET /healthz": {public: true},
		"GET /readyz":  {public: true},
		rpc(pbconnect.DuesServiceGetEviFilesProcedure):    {scope: ScopeRead, rpc: true, lease: true},
		rpc(pbconnect.DuesServiceGetPartiFilesProcedure):  {scope: ScopeRead, rpc: true, lease: true},
		rpc(pbconnect.DuesServiceGetIdxFilesProcedure):    {scope: ScopeRead, rpc: true, lease: true},
		rpc(pbconnect.DuesServiceSearchProcedure):         {scope: ScopeRead, rpc: true, lease: true},
		rpc(pbconnect.DuesServiceAppendIfExistsProcedure): {scope: ScopeUpload, rpc: true, lease: true},
		rpc(pbconnect.DuesServiceStreamFileProcedure):     {scope: ScopeUpload, rpc: true},
		"POST /web/upload/start":                          {scope: ScopeUpload},
		"POST /web/upload/chunk":                          {scope: ScopeUpload},
		"POST /web/upload/status":                         {scope: ScopeUpload},
		"POST /web/upload/abort":                          {scope: ScopeUpload},
		"POST /web/upload/finalize":                       {scope: ScopeUpload},
	}
	for p, rt := range s.registered {
		if got := (rs{rt.scope, rt.public, rt.rpc, rt.lease}); got != want[p] {
			t.Errorf("%s: %+v, want %+v", p, got, want[p])
		}
	}
	for p := range want {
		if _, ok := s.registered[p]; !ok {
			t.Errorf("%s is not registered", p)
		}
	}
}

func TestRouteMustBePublicOrScoped(t *testing.T) {
	t.Parallel()
	for _, rt := range []route{{}, {public: true, scope: ScopeRead}, {public: true, lease: true}} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("route %+v was registered", rt)
				}
			}()
			(&Server{registered: map[string]route{}, mux: http.NewServeMux()}).handle("GET /x", rt, http.NotFoundHandler())
		}()
	}
}

// A handler put on the mux without a scope is never served, whatever the
// token may do.
func TestUnscopedRouteRefused(t *testing.T) {
	t.Parallel()
	var ran atomic.Bool
	setup := func(s *Server) {
		s.mux.HandleFunc("GET /test/direct", func(http.ResponseWriter, *http.Request) { ran.Store(true) })
	}
	for _, cfg := range []Config{{}, {Tokens: []Credential{mustCredential(t, testToken, ScopeAll, "all")}}} {
		core, logs := observer.New(zap.WarnLevel)
		ls := startServer(t, cfg, Deps{Log: zap.New(core)}, setup)
		resp, _ := do(t, bearer(newReq(t, "GET", ls.url+"/test/direct", nil), testToken))
		if resp.StatusCode != http.StatusForbidden || ran.Load() {
			t.Errorf("tokens %d: %d, handler ran %v", len(cfg.Tokens), resp.StatusCode, ran.Load())
		}
		if n := logs.FilterMessage("server route has no scope").FilterField(zap.String("pattern", "GET /test/direct")).Len(); n != 1 {
			t.Errorf("tokens %d: %d warnings of the unscoped route, want 1", len(cfg.Tokens), n)
		}
	}
}

// h2cClient speaks cleartext HTTP/2, which gRPC needs, and sends token.
func h2cClient(t *testing.T, token string) *http.Client {
	t.Helper()
	var p http.Protocols
	p.SetUnencryptedHTTP2(true)
	tr := &http.Transport{Protocols: &p}
	t.Cleanup(tr.CloseIdleConnections)
	return &http.Client{Transport: bearerTransport{tr, token}}
}

type bearerTransport struct {
	base  http.RoundTripper
	token string
}

func (b bearerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	return b.base.RoundTrip(bearer(r.Clone(r.Context()), b.token))
}

// A token gets the routes its scopes name and no others, over every
// protocol; a refused RPC carries permission_denied and the missing scope.
func TestScopeMatrix(t *testing.T) {
	t.Parallel()
	reader, uploader := testToken40('r'), testToken40('u')
	core, logs := observer.New(zap.WarnLevel)
	h := openDB(t)
	if err := util.EnsureBlobPath(h.Root); err != nil {
		t.Fatal(err)
	}
	ls := startServer(t, Config{Tokens: []Credential{
		mustCredential(t, reader, ScopeRead, "reader"), mustCredential(t, uploader, ScopeUpload, "uploader"),
	}}, Deps{Handle: h, Log: zap.New(core)}, nil)
	ctx := context.Background()
	hash := plainHash(h.DB, []byte("not stored"))
	denied := func(what string, err error, scope string) {
		t.Helper()
		if connect.CodeOf(err) != connect.CodePermissionDenied || !strings.Contains(err.Error(), "may not "+scope) {
			t.Errorf("%s: %v, want permission_denied naming %s", what, err, scope)
		}
	}
	for _, proto := range []struct {
		name string
		opts []connect.ClientOption
	}{{"connect", nil}, {"grpc", []connect.ClientOption{connect.WithGRPC()}}, {"grpc-web", []connect.ClientOption{connect.WithGRPCWeb()}}} {
		r := pbconnect.NewDuesServiceClient(h2cClient(t, reader), ls.url, proto.opts...)
		u := pbconnect.NewDuesServiceClient(h2cClient(t, uploader), ls.url, proto.opts...)
		if _, err := r.GetEviFiles(ctx, connect.NewRequest(&pb.GetEviFilesReq{})); err != nil {
			t.Errorf("%s: reader GetEviFiles: %v", proto.name, err)
		}
		_, err := u.GetEviFiles(ctx, connect.NewRequest(&pb.GetEviFilesReq{}))
		denied(proto.name+": uploader GetEviFiles", err, "read")
		_, err = u.Search(ctx, connect.NewRequest(&pb.SearchReq{Keyword: "needle"}))
		denied(proto.name+": uploader Search", err, "read")
		res, err := u.AppendIfExists(ctx, connect.NewRequest(&pb.AppendIfExistsReq{FileHash: hash, FilePath: "/x"}))
		if err != nil || res.Msg.Exists {
			t.Errorf("%s: uploader AppendIfExists: %v %v", proto.name, res, err)
		}
		_, err = r.AppendIfExists(ctx, connect.NewRequest(&pb.AppendIfExistsReq{FileHash: hash, FilePath: "/x"}))
		denied(proto.name+": reader AppendIfExists", err, "upload")
		stream := r.StreamFile(ctx)
		_ = stream.Send(&pb.StreamFileReq{Payload: &pb.StreamFileReq_FileMeta{FileMeta: &pb.StreamFileMeta{FilePath: "/x", FileHash: hash}}})
		_, err = stream.CloseAndReceive()
		denied(proto.name+": reader StreamFile", err, "upload")
	}
	resp, body := do(t, bearer(newReq(t, "POST", ls.url+"/web/upload/start", strings.NewReader("{}")), reader))
	var eb errorBody
	_ = json.Unmarshal([]byte(body), &eb)
	if resp.StatusCode != http.StatusForbidden || !strings.Contains(eb.Error, "may not upload") || eb.RequestID == "" ||
		resp.Header.Get("WWW-Authenticate") != `Bearer realm="dues", error="insufficient_scope", scope="upload"` {
		t.Errorf("reader upload start: %d %s, challenge %q", resp.StatusCode, body, resp.Header.Get("WWW-Authenticate"))
	}
	if n := logs.FilterMessage("server scope refused").FilterField(zap.String("request_id", resp.Header.Get("Request-Id"))).
		FilterField(zap.String("path", "/web/upload/start")).FilterField(zap.String(logFieldBearer, "reader")).Len(); n != 1 {
		t.Errorf("%d refusals of the reader's upload start logged with its request and path, want 1", n)
	}
	if n := logs.FilterMessage("server scope refused").FilterField(zap.String(logFieldBearer, "uploader")).FilterField(zap.String("scope", "read")).Len(); n != 6 {
		t.Errorf("%d refusals of the uploader logged, want 6", n)
	}
	for _, e := range logs.All() {
		if s := fmt.Sprint(e.ContextMap()); strings.Contains(s, reader) || strings.Contains(s, uploader) {
			t.Fatalf("a token reached the log: %v", e.ContextMap())
		}
	}
}

// Export is never anonymous: without a token it is refused with how to set
// one; with tokens, only an export token gets it.
func TestExportScope(t *testing.T) {
	t.Parallel()
	setup := func(s *Server) {
		testRoute(s, "POST /test/export", ScopeExport, func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("in")) })
	}
	open := startServer(t, Config{}, Deps{}, setup)
	resp, body := do(t, newReq(t, "POST", open.url+"/test/export", nil))
	if resp.StatusCode != http.StatusForbidden || resp.Header.Get("WWW-Authenticate") != "" ||
		!strings.Contains(body, "--token-file") || !strings.Contains(body, TokenFileHeader) {
		t.Errorf("export without tokens: %d %s, challenge %q", resp.StatusCode, body, resp.Header.Get("WWW-Authenticate"))
	}
	reader, exporter := testToken40('r'), testToken40('e')
	ls := startServer(t, Config{Tokens: []Credential{
		mustCredential(t, reader, DefaultScopes, "reader"), mustCredential(t, exporter, ScopeAll, "exporter"),
	}}, Deps{}, setup)
	resp, _ = do(t, bearer(newReq(t, "POST", ls.url+"/test/export", nil), reader))
	if resp.StatusCode != http.StatusForbidden || !strings.Contains(resp.Header.Get("WWW-Authenticate"), `error="insufficient_scope", scope="export"`) {
		t.Errorf("export with a read,upload token: %d, challenge %q", resp.StatusCode, resp.Header.Get("WWW-Authenticate"))
	}
	if resp, body := do(t, bearer(newReq(t, "POST", ls.url+"/test/export", nil), exporter)); resp.StatusCode != 200 || body != "in" {
		t.Errorf("export with an all token: %d %s", resp.StatusCode, body)
	}
}

// The access log names a request's token by its label, also when its scope
// is refused, and has no label when no token is set or the token was
// refused.
func TestAccessLogBearer(t *testing.T) {
	t.Parallel()
	logged := func(ls *liveServer, logs *observer.ObservedLogs, token string) map[string]any {
		t.Helper()
		req := newReq(t, "POST", ls.url+"/web/upload/status", strings.NewReader("{}"))
		if token != "" {
			bearer(req, token)
		}
		resp, _ := do(t, req)
		e := logs.FilterMessage("server request").FilterField(zap.String("request_id", resp.Header.Get("Request-Id"))).All()
		if len(e) != 1 {
			t.Fatalf("%d access log entries", len(e))
		}
		return e[0].ContextMap()
	}
	core, logs := observer.New(zap.InfoLevel)
	reader := testToken40('r')
	ls := startServer(t, Config{Tokens: []Credential{
		mustCredential(t, testToken, ScopeUpload, "case-7"), mustCredential(t, reader, ScopeRead, "reader"),
	}}, Deps{Log: zap.New(core)}, nil)
	if got := logged(ls, logs, testToken)[logFieldBearer]; got != "case-7" {
		t.Errorf("bearer %v, want case-7", got)
	}
	if e := logged(ls, logs, reader); e[logFieldBearer] != "reader" || e["status"] != int64(http.StatusForbidden) {
		t.Errorf("a token refused its scope logged as %v, status %v", e[logFieldBearer], e["status"])
	}
	if got, ok := logged(ls, logs, testToken40('x'))[logFieldBearer]; ok {
		t.Errorf("a refused token logged as %v", got)
	}
	core, logs = observer.New(zap.InfoLevel)
	open := startServer(t, Config{}, Deps{Log: zap.New(core)}, nil)
	if got, ok := logged(open, logs, "")[logFieldBearer]; ok {
		t.Errorf("no token set, bearer %v", got)
	}
}

// An upload belongs to the token that started it: another token is told it
// does not exist, and the logs name the owner from start to store.
func TestUploadBelongsToItsToken(t *testing.T) {
	t.Parallel()
	alice, bob := testToken40('a'), testToken40('b')
	core, logs := observer.New(zap.InfoLevel)
	h := openDB(t)
	if err := util.EnsureBlobPath(h.Root); err != nil {
		t.Fatal(err)
	}
	ls := startServer(t, Config{Tokens: []Credential{
		mustCredential(t, alice, ScopeUpload, "alice"), mustCredential(t, bob, ScopeUpload, "bob"),
	}}, Deps{Handle: h, Log: zap.New(core)}, nil)
	data := bytes.Repeat([]byte("owned "), 1000)
	js := func(v any) *bytes.Reader { b, _ := json.Marshal(v); return bytes.NewReader(b) }
	startReq := bearer(newReq(t, "POST", ls.url+"/web/upload/start", js(map[string]any{
		"file_path": "/case/owned.bin", "file_hash": plainHash(h.DB, data), "size": len(data),
	})), alice)
	sr := post(t, startReq)
	id := sr.str("upload_id")
	if sr.status != 200 || id == "" {
		t.Fatalf("start: %d %v", sr.status, sr.body)
	}
	chunkReq := func(token string) reply {
		req := bearer(newReq(t, "POST", ls.url+"/web/upload/chunk", bytes.NewReader(data)), token)
		req.Header.Set("Upload-Id", id)
		req.Header.Set("Upload-Offset", "0")
		return post(t, req)
	}
	idReq := func(path, token string) reply {
		return post(t, bearer(newReq(t, "POST", ls.url+path, js(map[string]string{"upload_id": id})), token))
	}
	bobChunk := chunkReq(bob)
	if bobChunk.status != http.StatusNotFound {
		t.Errorf("bob's chunk: %d %v", bobChunk.status, bobChunk.body)
	}
	for _, p := range []string{"/web/upload/status", "/web/upload/abort", "/web/upload/finalize"} {
		if r := idReq(p, bob); r.status != http.StatusNotFound {
			t.Errorf("bob's %s: %d %v", p, r.status, r.body)
		}
	}
	if r := chunkReq(alice); r.status != 200 {
		t.Fatalf("alice's chunk: %d %v", r.status, r.body)
	}
	if r := idReq("/web/upload/finalize", alice); r.status != 200 || r.str("outcome") != "stored" {
		t.Fatalf("alice's finalize: %d %v", r.status, r.body)
	}
	started := logs.FilterMessage("upload started").FilterField(zap.String("upload_id", id)).All()
	if len(started) != 1 || started[0].ContextMap()[logFieldBearer] != "alice" || started[0].ContextMap()["request_id"] != sr.header.Get("Request-Id") {
		t.Errorf("upload started: %v, want bearer alice and request %s", started, sr.header.Get("Request-Id"))
	}
	if n := logs.FilterMessage("upload stored").FilterField(zap.String("upload_id", id)).FilterField(zap.String(logFieldBearer, "alice")).Len(); n != 1 {
		t.Errorf("%d upload stored lines name alice", n)
	}
	refused := logs.FilterMessage("upload of another token").FilterField(zap.String(logFieldBearer, "bob")).FilterField(zap.String("upload_id", id))
	if n := refused.Len(); n != 4 {
		t.Errorf("%d refusals of bob logged, want 4", n)
	}
	if n := refused.FilterField(zap.String("request_id", bobChunk.header.Get("Request-Id"))).Len(); n != 1 {
		t.Errorf("%d refusals logged with bob's chunk request, want 1", n)
	}

	// A failed upload names its token too.
	bad := post(t, bearer(newReq(t, "POST", ls.url+"/web/upload/start", js(map[string]any{
		"file_path": "/case/bad.bin", "file_hash": plainHash(h.DB, []byte("other content")), "size": len(data),
	})), alice))
	badID := bad.str("upload_id")
	badChunk := bearer(newReq(t, "POST", ls.url+"/web/upload/chunk", bytes.NewReader(data)), alice)
	badChunk.Header.Set("Upload-Id", badID)
	badChunk.Header.Set("Upload-Offset", "0")
	if r := post(t, badChunk); bad.status != 200 || r.status != 200 {
		t.Fatalf("bad upload: start %d, chunk %d %v", bad.status, r.status, r.body)
	}
	fin := post(t, bearer(newReq(t, "POST", ls.url+"/web/upload/finalize", js(map[string]string{"upload_id": badID})), alice))
	if fin.status != http.StatusUnprocessableEntity {
		t.Fatalf("bad finalize: %d %v", fin.status, fin.body)
	}
	if n := logs.FilterMessage("upload failed").FilterField(zap.String("upload_id", badID)).FilterField(zap.String(logFieldBearer, "alice")).Len(); n != 1 {
		t.Errorf("%d upload failed lines name alice", n)
	}

	// StreamFile names its token too.
	c := pbconnect.NewDuesServiceClient(h2cClient(t, bob), ls.url)
	streamed := []byte("streamed by bob")
	stream := c.StreamFile(context.Background())
	_ = stream.Send(&pb.StreamFileReq{Payload: &pb.StreamFileReq_FileMeta{FileMeta: &pb.StreamFileMeta{
		FilePath: "/case/bob.bin", FileHash: plainHash(h.DB, streamed), Size: int64(len(streamed)),
	}}})
	_ = stream.Send(&pb.StreamFileReq{Payload: &pb.StreamFileReq_File{File: streamed}})
	if _, err := stream.CloseAndReceive(); err != nil {
		t.Fatal(err)
	}
	if n := logs.FilterMessage("upload started").FilterField(zap.String(logFieldBearer, "bob")).Len(); n != 1 {
		t.Errorf("%d StreamFile starts name bob", n)
	}
}

// Without tokens, upload logs name no token.
func TestUploadLogsWithoutToken(t *testing.T) {
	t.Parallel()
	core, logs := observer.New(zap.InfoLevel)
	h := openDB(t)
	if err := util.EnsureBlobPath(h.Root); err != nil {
		t.Fatal(err)
	}
	ls := startServer(t, Config{}, Deps{Handle: h, Log: zap.New(core)}, nil)
	upload := func(path string, data, sent []byte, outcome int) {
		t.Helper()
		r := start(t, ls, path, "", plainHash(h.DB, data), int64(len(sent)))
		id := r.str("upload_id")
		if r.status != 200 {
			t.Fatalf("start: %d %v", r.status, r.body)
		}
		if r := chunk(t, ls, id, 0, sent); r.status != 200 {
			t.Fatalf("chunk: %d %v", r.status, r.body)
		}
		if r := postJSON(t, ls, "/web/upload/finalize", map[string]string{"upload_id": id}); r.status != outcome {
			t.Fatalf("finalize: %d %v, want %d", r.status, r.body, outcome)
		}
	}
	data := []byte("anonymous upload")
	upload("/case/anon.bin", data, data, http.StatusOK)
	upload("/case/bad.bin", []byte("other content"), data, http.StatusUnprocessableEntity)
	for _, msg := range []string{"upload started", "upload stored", "upload failed"} {
		entries := logs.FilterMessage(msg).All()
		if len(entries) == 0 {
			t.Errorf("no %s line", msg)
		}
		for _, e := range entries {
			if _, ok := e.ContextMap()[logFieldBearer]; ok {
				t.Errorf("%s names a token: %v", msg, e.ContextMap())
			}
		}
	}
}

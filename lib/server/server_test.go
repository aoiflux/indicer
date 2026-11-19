package server

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"indicer/lib/dbopen"

	"github.com/dgraph-io/badger/v4"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

const testToken = "0123456789abcdef0123456789abcdef-token"

// openDB opens a new database the way `dues server` does.
func openDB(t *testing.T) *dbopen.Handle {
	t.Helper()
	h, err := dbopen.Open(context.Background(), dbopen.Request{Path: filepath.Join(t.TempDir(), "db"), Mode: dbopen.ReadWrite})
	if err != nil {
		t.Fatal(err)
	}
	// A server that stopped cleanly has closed it already; Close is then a
	// no-op.
	t.Cleanup(func() { _ = h.Close() })
	return h
}

// wantReopens checks that the database at root opens writable: whoever had
// it open has let it go.
func wantReopens(t *testing.T, root string) {
	t.Helper()
	h, err := dbopen.Open(context.Background(), dbopen.Request{Path: root, Mode: dbopen.ReadWrite})
	if err != nil {
		t.Fatalf("the database does not reopen writable: %v", err)
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
}

func testLimits() Limits {
	l := DefaultLimits()
	l.ShutdownGrace = 300 * time.Millisecond
	l.IdleWait = 300 * time.Millisecond
	l.PauseLeaseWait = 300 * time.Millisecond
	l.ResumeRetry = [resumeRetries]time.Duration{time.Millisecond, time.Millisecond, time.Millisecond}
	l.Export.DownloadIdle = 2 * time.Second
	l.Export.SizingBudget = 200 * time.Millisecond
	// Tests reap when they mean to.
	l.Export.ReapInterval = time.Hour
	return l
}

type liveServer struct {
	*Server
	url    string
	cancel context.CancelFunc
	done   chan error
}

// startServer serves cfg on a free loopback port. setup, when given, runs
// before listening; tests add routes there.
func startServer(t *testing.T, cfg Config, deps Deps, setup func(*Server)) *liveServer {
	t.Helper()
	if cfg.Listen == nil {
		cfg.Listen = []string{"127.0.0.1:0"}
	}
	if cfg.Limits == (Limits{}) {
		cfg.Limits = testLimits()
	}
	if deps.Handle == nil {
		deps.Handle = openDB(t)
	}
	s, err := New(cfg, deps)
	if err != nil {
		t.Fatal(err)
	}
	if setup != nil {
		setup(s)
	}
	if err := s.Listen(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	ls := &liveServer{Server: s, url: "http://" + s.Addrs()[0].String(), cancel: cancel, done: make(chan error, 1)}
	go func() { ls.done <- s.Serve(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-ls.done:
		case <-time.After(15 * time.Second):
			t.Error("the server did not stop")
		}
	})
	return ls
}

// db is the database the server has open now.
func (ls *liveServer) db() *badger.DB { return ls.dbs.current().db }

// stop stops the server and returns what Serve returned.
func (ls *liveServer) stop(t *testing.T) error {
	t.Helper()
	ls.cancel()
	select {
	case err := <-ls.done:
		ls.done <- err // for the cleanup
		return err
	case <-time.After(15 * time.Second):
		t.Fatal("the server did not stop")
		return nil
	}
}

func do(t *testing.T, req *http.Request) (*http.Response, string) {
	t.Helper()
	resp, err := testClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

func newReq(t *testing.T, method, url string, body io.Reader) *http.Request {
	t.Helper()
	req, err := http.NewRequest(method, url, body)
	if err != nil {
		t.Fatal(err)
	}
	return req
}

func TestConfigValidate(t *testing.T) {
	t.Parallel()
	tok := mustCredential(t, testToken, DefaultScopes, "test")
	var tooMany []Credential
	for i := range MaxTokens + 1 {
		tooMany = append(tooMany, mustCredential(t, fmt.Sprintf("%s%04d", testToken, i), ScopeRead, fmt.Sprintf("t%d", i)))
	}
	for _, c := range []struct {
		name    string
		cfg     Config
		wantErr string
	}{
		{"default", Config{}, ""},
		{"loopback v4", Config{Listen: []string{"127.0.0.1:0"}}, ""},
		{"loopback other v4", Config{Listen: []string{"127.0.0.2:8080"}}, ""},
		{"loopback v6", Config{Listen: []string{"[::1]:0"}}, ""},
		{"localhost", Config{Listen: []string{"localhost:50051"}}, ""},
		{"all interfaces", Config{Listen: []string{":50051"}}, "needs a token"},
		{"any v4", Config{Listen: []string{"0.0.0.0:50051"}}, "needs a token"},
		{"any v6", Config{Listen: []string{"[::]:50051"}}, "needs a token"},
		{"lan", Config{Listen: []string{"192.168.1.5:50051"}}, "needs a token"},
		{"name", Config{Listen: []string{"example.com:50051"}}, "needs a token"},
		{"lan with token", Config{Listen: []string{"192.168.1.5:50051"}, Tokens: []Credential{tok}}, ""},
		{"lan with a zero credential", Config{Listen: []string{"192.168.1.5:50051"}, Tokens: []Credential{{}}}, "not made by NewCredential"},
		{"same token twice", Config{Tokens: []Credential{tok, mustCredential(t, testToken, ScopeRead, "other")}}, "same token"},
		{"same label but for case", Config{Tokens: []Credential{tok, mustCredential(t, testToken40('x'), ScopeRead, "TEST")}}, "same label"},
		{"too many tokens", Config{Tokens: tooMany}, "at most 64"},
		{"the most tokens", Config{Tokens: tooMany[:MaxTokens]}, ""},
		{"same label, upper case first", Config{Tokens: []Credential{mustCredential(t, testToken40('y'), ScopeRead, "TEST"), tok}}, "same label"},
		{"credential without a label", Config{Tokens: []Credential{anonymous}}, "not made by NewCredential"},
		{"no port", Config{Listen: []string{"127.0.0.1"}}, "host:port"},
		{"bad port", Config{Listen: []string{"127.0.0.1:70000"}}, "port"},
		{"twice", Config{Listen: []string{"127.0.0.1:1", "127.0.0.1:1"}}, "twice"},
		{"origin", Config{CORSOrigins: []string{"https://app.example.org", "http://10.0.0.2:8080"}}, ""},
		{"origin without scheme", Config{CORSOrigins: []string{"app.example.org"}}, "CORS origin"},
		{"origin with path", Config{CORSOrigins: []string{"https://app.example.org/ui"}}, "CORS origin"},
		{"wildcard origin", Config{CORSOrigins: []string{"*"}}, "CORS origin"},
		{"null origin", Config{CORSOrigins: []string{"null"}}, "CORS origin"},
	} {
		t.Run(c.name, func(t *testing.T) {
			err := c.cfg.Validate()
			switch {
			case c.wantErr == "" && err != nil:
				t.Fatalf("Validate: %v", err)
			case c.wantErr != "" && (err == nil || !strings.Contains(err.Error(), c.wantErr)):
				t.Fatalf("Validate: %v, want an error containing %q", err, c.wantErr)
			}
		})
	}
}

// SRV-1: with no --listen the server is reachable only on loopback.
func TestDefaultAddressesAreLoopback(t *testing.T) {
	t.Parallel()
	addrs, err := parseListen(nil)
	if err != nil {
		t.Fatal(err)
	}
	want := []listenAddr{{"127.0.0.1:50051", true, false}, {"[::1]:50051", true, true}}
	if fmt.Sprint(addrs) != fmt.Sprint(want) {
		t.Fatalf("default addresses %v, want %v", addrs, want)
	}
}

// The server speaks HTTP/1.1 and cleartext HTTP/2 with prior knowledge, the
// transport gRPC and Connect clients use against a plain-text listener.
func TestHTTPServerProtocols(t *testing.T) {
	t.Parallel()
	ls := startServer(t, Config{}, Deps{}, func(s *Server) {
		testRoute(s, "GET /test/proto", ScopeRead, func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, r.Proto) })
	})
	for _, tc := range []struct {
		name  string
		proto func(*http.Protocols)
		want  string
	}{
		{"http1", func(p *http.Protocols) { p.SetHTTP1(true) }, "HTTP/1.1"},
		{"h2c prior knowledge", func(p *http.Protocols) { p.SetUnencryptedHTTP2(true) }, "HTTP/2.0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var p http.Protocols
			tc.proto(&p)
			tr := &http.Transport{Protocols: &p}
			defer tr.CloseIdleConnections()
			resp, err := (&http.Client{Transport: tr}).Get(ls.url + "/test/proto")
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			body, _ := io.ReadAll(resp.Body)
			if string(body) != tc.want {
				t.Fatalf("server saw %q, want %q", body, tc.want)
			}
		})
	}
}

// deadlineWriter records the deadlines set through its
// ResponseController.
type deadlineWriter struct {
	http.ResponseWriter
	read, write []time.Time
}

func (w *deadlineWriter) SetReadDeadline(t time.Time) error {
	w.read = append(w.read, t)
	return nil
}

func (w *deadlineWriter) SetWriteDeadline(t time.Time) error {
	w.write = append(w.write, t)
	return nil
}

// TestIdleBodyClearsDeadlineAtEnd: each read of a body moves its deadline
// on, and its end clears it, so no deadline is left to cancel a request
// that waits long after its body, as a StreamFile through a pause does.
func TestIdleBodyClearsDeadlineAtEnd(t *testing.T) {
	t.Parallel()
	w := &deadlineWriter{ResponseWriter: httptest.NewRecorder()}
	b := &idleBody{ReadCloser: io.NopCloser(strings.NewReader("body")), rc: http.NewResponseController(w), idle: time.Minute}
	if _, err := io.ReadAll(b); err != nil {
		t.Fatal(err)
	}
	if len(w.read) < 2 || !w.read[len(w.read)-1].IsZero() {
		t.Fatalf("read deadlines %v: want the last one cleared", w.read)
	}
	for _, d := range w.read[:len(w.read)-1] {
		if d.IsZero() {
			t.Errorf("read deadlines %v: one was cleared before the end", w.read)
		}
	}
}

// TestIdleWriterMovesDeadline: each write of a response moves its deadline
// a full idle period past the moment it began, so a client that keeps
// reading is never cut.
func TestIdleWriterMovesDeadline(t *testing.T) {
	t.Parallel()
	const idle = time.Minute
	rec := httptest.NewRecorder()
	w := &deadlineWriter{ResponseWriter: rec}
	iw := &idleWriter{ResponseWriter: w, rc: http.NewResponseController(w), idle: idle}
	for i := range 3 {
		before := time.Now()
		if _, err := io.WriteString(iw, "part"); err != nil {
			t.Fatal(err)
		}
		if len(w.write) != i+1 || w.write[i].Before(before.Add(idle)) {
			t.Fatalf("write %d: deadlines %v, want one at least %v after %v", i, w.write, idle, before)
		}
	}
	if rec.Body.String() != "partpartpart" {
		t.Errorf("written %q", rec.Body.String())
	}
}

// TestIdleWriterCut: once cut, a response takes no more writes, and its
// deadline is in the past, so a write already blocked returns.
func TestIdleWriterCut(t *testing.T) {
	t.Parallel()
	rec := httptest.NewRecorder()
	w := &deadlineWriter{ResponseWriter: rec}
	iw := &idleWriter{ResponseWriter: w, rc: http.NewResponseController(w), idle: time.Minute}
	if _, err := io.WriteString(iw, "before"); err != nil {
		t.Fatal(err)
	}
	iw.cut()
	cutAt := time.Now()
	if n, err := io.WriteString(iw, "after"); n != 0 || !errors.Is(err, errResponseCut) {
		t.Errorf("a write after the cut: %d, %v", n, err)
	}
	if rec.Body.String() != "before" {
		t.Errorf("written %q", rec.Body.String())
	}
	if last := w.write[len(w.write)-1]; last.After(cutAt) {
		t.Errorf("the last deadline %v is after the cut at %v", last, cutAt)
	}
	if iw.Unwrap() != w {
		t.Error("Unwrap does not reach the writer below")
	}
}

// TestIdleWriterCutAfterEnd: a cut that fires once the handler returned
// leaves the response writer alone. HTTP/2 invalidates the writer when the
// handler returns, and a deadline set on it then crashes the server.
func TestIdleWriterCutAfterEnd(t *testing.T) {
	t.Parallel()
	w := &deadlineWriter{ResponseWriter: httptest.NewRecorder()}
	iw := &idleWriter{ResponseWriter: w, rc: http.NewResponseController(w), idle: time.Minute}
	iw.end()
	iw.cut()
	if len(w.write) != 0 {
		t.Errorf("deadlines %v set after the handler returned", w.write)
	}
}

// SRV-1: only the health checks answer outside the service; "/" is no
// longer a banner.
func TestHealthAndUnknownPaths(t *testing.T) {
	t.Parallel()
	ls := startServer(t, Config{}, Deps{}, nil)
	for _, c := range []struct {
		method, path string
		status       int
		body         string
	}{
		{"GET", "/healthz", 200, "ok\n"},
		{"GET", "/readyz", 200, "ready\n"},
		{"GET", "/", 404, ""},
		{"GET", "/nope", 404, ""},
		{"GET", "/web/upload/start", 405, ""},
		{"POST", "/healthz", 405, ""},
	} {
		resp, body := do(t, newReq(t, c.method, ls.url+c.path, nil))
		if resp.StatusCode != c.status || (c.body != "" && body != c.body) {
			t.Errorf("%s %s: %d %q, want %d %q", c.method, c.path, resp.StatusCode, body, c.status, c.body)
		}
		if resp.Header.Get("Request-Id") == "" {
			t.Errorf("%s %s: no Request-Id", c.method, c.path)
		}
	}
}

// SRV-1: a token, once set, is required on every request but health checks.
func TestBearerToken(t *testing.T) {
	t.Parallel()
	ls := startServer(t, Config{Tokens: []Credential{mustCredential(t, testToken, ScopeRead, "test")}}, Deps{}, func(s *Server) {
		testRoute(s, "POST /test/echo", ScopeRead, func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "in") })
	})
	for _, c := range []struct {
		name, auth string
		status     int
	}{
		{"none", "", 401},
		{"wrong", "Bearer " + strings.Repeat("x", len(testToken)), 401},
		{"prefix", "Bearer " + testToken[:32], 401},
		{"basic", "Basic " + testToken, 401},
		{"right", "Bearer " + testToken, 200},
		{"scheme case", "bearer " + testToken, 200},
		{"extra space", "Bearer  " + testToken, 200},
	} {
		req := newReq(t, "POST", ls.url+"/test/echo", nil)
		if c.auth != "" {
			req.Header.Set("Authorization", c.auth)
		}
		resp, _ := do(t, req)
		if resp.StatusCode != c.status {
			t.Errorf("%s: %d, want %d", c.name, resp.StatusCode, c.status)
		}
		if c.status == 401 && !strings.HasPrefix(resp.Header.Get("WWW-Authenticate"), "Bearer") {
			t.Errorf("%s: no WWW-Authenticate challenge", c.name)
		}
	}
	for _, p := range []string{"/healthz", "/readyz"} {
		if resp, _ := do(t, newReq(t, "GET", ls.url+p, nil)); resp.StatusCode != 200 {
			t.Errorf("%s without a token: %d", p, resp.StatusCode)
		}
	}
	// What matches no route is answered by the mux, once the token is right.
	for _, c := range []struct {
		method, path, auth string
		status             int
	}{
		{"GET", "/nope", "", 401},
		{"POST", "/healthz", "", 401},
		{"GET", "/nope", "Bearer " + testToken, 404},
		{"GET", "/test/echo", "Bearer " + testToken, 405},
	} {
		req := newReq(t, c.method, ls.url+c.path, nil)
		if c.auth != "" {
			req.Header.Set("Authorization", c.auth)
		}
		resp, _ := do(t, req)
		if resp.StatusCode != c.status || (c.status == 405 && resp.Header.Get("Allow") == "") {
			t.Errorf("%s %s (%q): %d, Allow %q, want %d", c.method, c.path, c.auth, resp.StatusCode, resp.Header.Get("Allow"), c.status)
		}
	}
	// A preflight carries no credentials; it is answered before auth.
	req := newReq(t, "OPTIONS", ls.url+"/test/echo", nil)
	req.Header.Set("Origin", "http://localhost:3000")
	req.Header.Set("Access-Control-Request-Method", "POST")
	if resp, _ := do(t, req); resp.StatusCode != http.StatusNoContent {
		t.Errorf("preflight without a token: %d", resp.StatusCode)
	}
}

// SRV-1: on a loopback address only localhost names are answered, so a page
// whose name was rebound to 127.0.0.1 cannot reach the server.
func TestHostCheckDefeatsRebinding(t *testing.T) {
	t.Parallel()
	ls := startServer(t, Config{}, Deps{}, nil)
	for host, status := range map[string]int{
		"attacker.example":       421,
		"attacker.example:50051": 421,
		"localhost.attacker.io":  421,
		"10.0.0.1:50051":         421,
		"localhost":              200,
		"LOCALHOST:50051":        200,
		"app.localhost:50051":    200,
		"127.0.0.1:50051":        200,
		"127.8.9.10":             200,
		"[::1]:50051":            200,
	} {
		req := newReq(t, "GET", ls.url+"/healthz", nil)
		req.Host = host
		if resp, _ := do(t, req); resp.StatusCode != status {
			t.Errorf("Host %q: %d, want %d", host, resp.StatusCode, status)
		}
	}
}

// SRV-1: loopback pages on any port and the configured origins may call the
// server; any other origin is refused, preflight or not.
func TestCORS(t *testing.T) {
	t.Parallel()
	ls := startServer(t, Config{CORSOrigins: []string{"https://app.example.org"}}, Deps{}, func(s *Server) {
		testRoute(s, "POST /test/echo", ScopeRead, func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "in") })
	})
	allowed := []string{
		"http://localhost:3000", "http://127.0.0.1:8080", "https://[::1]:8443", "http://app.localhost",
		"https://app.example.org", "https://APP.example.org:443",
	}
	refused := []string{
		"https://evil.example", "https://app.example.org:8443", "http://app.example.org",
		"http://localhost.evil.example", "null", "file://", "chrome-extension://abc",
	}
	for _, o := range allowed {
		pre := newReq(t, "OPTIONS", ls.url+"/test/echo", nil)
		pre.Header.Set("Origin", o)
		pre.Header.Set("Access-Control-Request-Method", "POST")
		pre.Header.Set("Access-Control-Request-Headers", "authorization, content-type")
		resp, _ := do(t, pre)
		if resp.StatusCode != http.StatusNoContent || resp.Header.Get("Access-Control-Allow-Origin") != o ||
			!strings.Contains(resp.Header.Get("Access-Control-Allow-Headers"), "Authorization") ||
			!strings.Contains(resp.Header.Get("Access-Control-Allow-Headers"), "If-Range") {
			t.Errorf("preflight from %s: %d, allow-origin %q", o, resp.StatusCode, resp.Header.Get("Access-Control-Allow-Origin"))
		}
		req := newReq(t, "POST", ls.url+"/test/echo", strings.NewReader("x"))
		req.Header.Set("Origin", o)
		resp, body := do(t, req)
		if resp.StatusCode != 200 || body != "in" || resp.Header.Get("Access-Control-Allow-Origin") != o ||
			!strings.Contains(resp.Header.Get("Access-Control-Expose-Headers"), "Request-Id") ||
			!strings.Contains(resp.Header.Get("Access-Control-Expose-Headers"), "Repr-Digest") {
			t.Errorf("POST from %s: %d %q", o, resp.StatusCode, body)
		}
	}
	for _, o := range refused {
		pre := newReq(t, "OPTIONS", ls.url+"/test/echo", nil)
		pre.Header.Set("Origin", o)
		pre.Header.Set("Access-Control-Request-Method", "POST")
		if resp, _ := do(t, pre); resp.StatusCode != http.StatusForbidden || resp.Header.Get("Access-Control-Allow-Origin") != "" {
			t.Errorf("preflight from %s: %d, allow-origin %q", o, resp.StatusCode, resp.Header.Get("Access-Control-Allow-Origin"))
		}
		// A simple request needs no preflight; it is refused before it is
		// served (CSRF).
		req := newReq(t, "POST", ls.url+"/test/echo", strings.NewReader("x"))
		req.Header.Set("Origin", o)
		req.Header.Set("Content-Type", "text/plain")
		if resp, body := do(t, req); resp.StatusCode != http.StatusForbidden || body == "in" {
			t.Errorf("POST from %s: %d %q", o, resp.StatusCode, body)
		}
	}
	// No Origin: not from a page; served, with no CORS grant.
	resp, body := do(t, newReq(t, "POST", ls.url+"/test/echo", nil))
	if resp.StatusCode != 200 || body != "in" || resp.Header.Get("Access-Control-Allow-Origin") != "" {
		t.Errorf("POST without Origin: %d %q, allow-origin %q", resp.StatusCode, body, resp.Header.Get("Access-Control-Allow-Origin"))
	}
}

// JSON request bodies are bounded.
func TestJSONBodyLimit(t *testing.T) {
	t.Parallel()
	ls := startServer(t, Config{}, Deps{}, nil)
	body := `{"file_path":"` + strings.Repeat("a", 64<<10) + `"}`
	resp, msg := do(t, newReq(t, "POST", ls.url+"/web/upload/start", strings.NewReader(body)))
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("a %d-byte JSON body: %d %s", len(body), resp.StatusCode, msg)
	}
}

// SRV-1: requests beyond the in-flight limit are refused, not queued.
func TestInFlightLimit(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	entered := make(chan struct{}, 1)
	lim := testLimits()
	lim.MaxInFlight = 1
	ls := startServer(t, Config{Limits: lim}, Deps{}, func(s *Server) {
		testRoute(s, "GET /test/block", ScopeRead, func(w http.ResponseWriter, r *http.Request) {
			entered <- struct{}{}
			<-release
		})
	})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		resp, err := testClient.Get(ls.url + "/test/block")
		if err == nil {
			resp.Body.Close()
		}
	}()
	<-entered
	resp, _ := do(t, newReq(t, "GET", ls.url+"/healthz", nil))
	if resp.StatusCode != http.StatusServiceUnavailable || resp.Header.Get("Retry-After") == "" {
		t.Errorf("request over the limit: %d, Retry-After %q", resp.StatusCode, resp.Header.Get("Retry-After"))
	}
	close(release)
	wg.Wait()
	if resp, _ := do(t, newReq(t, "GET", ls.url+"/healthz", nil)); resp.StatusCode != 200 {
		t.Errorf("after the first request ended: %d", resp.StatusCode)
	}
}

// SRV-1: connections beyond the limit wait until one closes.
func TestConnectionLimit(t *testing.T) {
	t.Parallel()
	lim := testLimits()
	lim.MaxConns = 1
	ls := startServer(t, Config{Limits: lim}, Deps{}, nil)
	held, err := net.Dial("tcp", ls.Addrs()[0].String())
	if err != nil {
		t.Fatal(err)
	}
	// Accept the held connection before trying a second.
	deadline := time.Now().Add(5 * time.Second)
	for len(ls.conns) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	client := &http.Client{Timeout: 500 * time.Millisecond, Transport: &http.Transport{DisableKeepAlives: true}}
	if resp, err := client.Get(ls.url + "/healthz"); err == nil {
		resp.Body.Close()
		t.Fatalf("a second connection was served (%d) while the limit was held", resp.StatusCode)
	}
	held.Close()
	client.Timeout = 5 * time.Second
	resp, err := client.Get(ls.url + "/healthz")
	if err != nil {
		t.Fatalf("after the held connection closed: %v", err)
	}
	resp.Body.Close()
}

// SRV-1: a client that never finishes its headers is cut off.
func TestSlowHeadersAreCut(t *testing.T) {
	t.Parallel()
	lim := testLimits()
	lim.ReadHeaderTimeout = 200 * time.Millisecond
	ls := startServer(t, Config{Limits: lim}, Deps{}, nil)
	c, err := net.Dial("tcp", ls.Addrs()[0].String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := io.WriteString(c, "GET /healthz HTTP/1.1\r\nHost: localhost\r\n"); err != nil {
		t.Fatal(err)
	}
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	start := time.Now()
	_, err = bufio.NewReader(c).ReadString('\n')
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		t.Fatal("the server kept a connection whose headers never ended")
	}
	if time.Since(start) > 3*time.Second {
		t.Fatalf("cut off after %v", time.Since(start))
	}
}

// A handler panic is a logged 500 with the request ID; the server goes on.
func TestPanicBecomes500(t *testing.T) {
	t.Parallel()
	core, logs := observer.New(zap.InfoLevel)
	ls := startServer(t, Config{}, Deps{Log: zap.New(core)}, func(s *Server) {
		testRoute(s, "GET /test/panic", ScopeRead, func(http.ResponseWriter, *http.Request) { panic("boom") })
	})
	resp, body := do(t, newReq(t, "GET", ls.url+"/test/panic", nil))
	id := resp.Header.Get("Request-Id")
	if resp.StatusCode != 500 || id == "" || !strings.Contains(body, id) || strings.Contains(body, "boom") {
		t.Fatalf("panic: %d %q (request %s)", resp.StatusCode, body, id)
	}
	if n := logs.FilterMessage("server PANIC").FilterField(zap.String("request_id", id)).Len(); n != 1 {
		t.Errorf("%d panic log entries for the request", n)
	}
	if resp, _ := do(t, newReq(t, "GET", ls.url+"/healthz", nil)); resp.StatusCode != 200 {
		t.Errorf("after a panic: %d", resp.StatusCode)
	}
}

// The access log has one line per request, with its ID, and never the token.
func TestAccessLogOmitsToken(t *testing.T) {
	t.Parallel()
	core, logs := observer.New(zap.InfoLevel)
	ls := startServer(t, Config{Tokens: []Credential{mustCredential(t, testToken, ScopeRead, "test")}}, Deps{Log: zap.New(core)}, nil)
	req := newReq(t, "GET", ls.url+"/readyz?x=1", nil)
	req.Header.Set("Authorization", "Bearer "+testToken)
	resp, _ := do(t, req)
	id := resp.Header.Get("Request-Id")
	entries := logs.FilterMessage("server request").FilterField(zap.String("request_id", id)).All()
	if len(entries) != 1 {
		t.Fatalf("%d access log entries for the request", len(entries))
	}
	if got := entries[0].ContextMap()["status"]; fmt.Sprint(got) != "200" {
		t.Errorf("logged status %v", got)
	}
	for _, e := range logs.All() {
		if strings.Contains(fmt.Sprint(e.ContextMap()), testToken) || strings.Contains(e.Message, testToken) {
			t.Fatalf("the token reached the log: %v", e.ContextMap())
		}
	}
}

// SRV-7: a stop lets requests finish, then cancels them; when they return,
// the stop is clean.
func TestShutdownCancelsRequests(t *testing.T) {
	t.Parallel()
	entered := make(chan struct{}, 1)
	ls := startServer(t, Config{}, Deps{}, func(s *Server) {
		testRoute(s, "GET /test/wait", ScopeRead, func(w http.ResponseWriter, r *http.Request) {
			entered <- struct{}{}
			<-r.Context().Done()
		})
	})
	go func() {
		if resp, err := testClient.Get(ls.url + "/test/wait"); err == nil {
			resp.Body.Close()
		}
	}()
	<-entered
	start := time.Now()
	if err := ls.stop(t); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if took := time.Since(start); took > stopBound {
		t.Errorf("stop took %v", took)
	}
	if ls.work.count() != 0 {
		t.Errorf("%d requests still counted", ls.work.count())
	}
}

// SRV-7: work that does not stop when told makes the stop unclean, so the
// database is not closed under it.
func TestShutdownWithStuckWorkIsUnclean(t *testing.T) {
	t.Parallel()
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	ls := startServer(t, Config{}, Deps{}, func(s *Server) {
		testRoute(s, "GET /test/stuck", ScopeRead, func(w http.ResponseWriter, r *http.Request) {
			entered <- struct{}{}
			<-release
		})
	})
	defer close(release)
	go func() {
		if resp, err := testClient.Get(ls.url + "/test/stuck"); err == nil {
			resp.Body.Close()
		}
	}()
	<-entered
	if err := ls.stop(t); !errors.Is(err, ErrUncleanShutdown) {
		t.Fatalf("stop with stuck work: %v, want ErrUncleanShutdown", err)
	}
}

// Work started by a request (an upload's ingest) counts too, and is told to
// stop through its context.
func TestShutdownWaitsForWork(t *testing.T) {
	t.Parallel()
	stopped := make(chan error, 1)
	ls := startServer(t, Config{}, Deps{}, nil)
	ls.goWork(func(ctx context.Context) {
		<-ctx.Done()
		stopped <- context.Cause(ctx)
	})
	if err := ls.stop(t); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if err := <-stopped; !errors.Is(err, errStopping) {
		t.Errorf("work stopped with %v", err)
	}
}

// A draining server is not ready and takes no new requests.
func TestDrainingRefusesRequests(t *testing.T) {
	t.Parallel()
	h := openDB(t)
	s, err := New(Config{Limits: testLimits()}, Deps{Handle: h})
	if err != nil {
		t.Fatal(err)
	}
	s.draining.Store(true)
	srv := httptestServer(t, s)
	if resp, _ := do(t, newReq(t, "GET", srv+"/readyz", nil)); resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("readyz while draining: %d", resp.StatusCode)
	}
	if resp, _ := do(t, newReq(t, "POST", srv+"/web/upload/start", strings.NewReader("{}"))); resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("a request while draining: %d", resp.StatusCode)
	}
	if resp, _ := do(t, newReq(t, "GET", srv+"/healthz", nil)); resp.StatusCode != 200 {
		t.Errorf("healthz while draining: %d", resp.StatusCode)
	}
}

// httptestServer serves s's handler on a loopback listener of its own.
func httptestServer(t *testing.T, s *Server) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	hs := s.newHTTPServer(s.handler())
	go func() { _ = hs.Serve(ln) }()
	t.Cleanup(func() { _ = hs.Close() })
	return "http://" + ln.Addr().String()
}

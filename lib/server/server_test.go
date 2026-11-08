package server

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"indicer/lib/dbopen"

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
	t.Cleanup(func() {
		if !h.DB.IsClosed() {
			_ = h.Close()
		}
	})
	return h
}

func testLimits() Limits {
	l := DefaultLimits()
	l.ShutdownGrace = 300 * time.Millisecond
	l.IdleWait = 300 * time.Millisecond
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
	if deps.DB == nil {
		h := openDB(t)
		deps.DB, deps.Root = h.DB, h.Root
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
	resp, err := http.DefaultClient.Do(req)
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
		{"lan with token", Config{Listen: []string{"192.168.1.5:50051"}, Token: testToken}, ""},
		{"short token", Config{Listen: []string{"192.168.1.5:50051"}, Token: testToken[:31]}, "at least 32"},
		{"short token on loopback", Config{Token: "short"}, "at least 32"},
		{"token with space", Config{Token: strings.Repeat("a", 20) + " " + strings.Repeat("b", 20)}, "visible ASCII"},
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
	ls := startServer(t, Config{}, Deps{}, func(s *Server) {
		s.mux.HandleFunc("GET /test/proto", func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, r.Proto) })
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

// SRV-1: only the health checks answer outside the service; "/" is no
// longer a banner.
func TestHealthAndUnknownPaths(t *testing.T) {
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
	ls := startServer(t, Config{Token: testToken}, Deps{}, func(s *Server) {
		s.mux.HandleFunc("POST /test/echo", func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "in") })
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
	ls := startServer(t, Config{CORSOrigins: []string{"https://app.example.org"}}, Deps{}, func(s *Server) {
		s.mux.HandleFunc("POST /test/echo", func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "in") })
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
			!strings.Contains(resp.Header.Get("Access-Control-Allow-Headers"), "Authorization") {
			t.Errorf("preflight from %s: %d, allow-origin %q", o, resp.StatusCode, resp.Header.Get("Access-Control-Allow-Origin"))
		}
		req := newReq(t, "POST", ls.url+"/test/echo", strings.NewReader("x"))
		req.Header.Set("Origin", o)
		resp, body := do(t, req)
		if resp.StatusCode != 200 || body != "in" || resp.Header.Get("Access-Control-Allow-Origin") != o ||
			!strings.Contains(resp.Header.Get("Access-Control-Expose-Headers"), "Request-Id") {
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
	ls := startServer(t, Config{}, Deps{}, nil)
	body := `{"file_path":"` + strings.Repeat("a", 64<<10) + `"}`
	resp, msg := do(t, newReq(t, "POST", ls.url+"/web/upload/start", strings.NewReader(body)))
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("a %d-byte JSON body: %d %s", len(body), resp.StatusCode, msg)
	}
}

// SRV-1: requests beyond the in-flight limit are refused, not queued.
func TestInFlightLimit(t *testing.T) {
	release := make(chan struct{})
	entered := make(chan struct{}, 1)
	lim := testLimits()
	lim.MaxInFlight = 1
	ls := startServer(t, Config{Limits: lim}, Deps{}, func(s *Server) {
		s.mux.HandleFunc("GET /test/block", func(w http.ResponseWriter, r *http.Request) {
			entered <- struct{}{}
			<-release
		})
	})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		resp, err := http.Get(ls.url + "/test/block")
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
	core, logs := observer.New(zap.InfoLevel)
	ls := startServer(t, Config{}, Deps{Log: zap.New(core)}, func(s *Server) {
		s.mux.HandleFunc("GET /test/panic", func(http.ResponseWriter, *http.Request) { panic("boom") })
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
	core, logs := observer.New(zap.InfoLevel)
	ls := startServer(t, Config{Token: testToken}, Deps{Log: zap.New(core)}, nil)
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
	entered := make(chan struct{}, 1)
	ls := startServer(t, Config{}, Deps{}, func(s *Server) {
		s.mux.HandleFunc("GET /test/wait", func(w http.ResponseWriter, r *http.Request) {
			entered <- struct{}{}
			<-r.Context().Done()
		})
	})
	go func() {
		if resp, err := http.Get(ls.url + "/test/wait"); err == nil {
			resp.Body.Close()
		}
	}()
	<-entered
	start := time.Now()
	if err := ls.stop(t); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if took := time.Since(start); took > 3*time.Second {
		t.Errorf("stop took %v", took)
	}
	if ls.work.count() != 0 {
		t.Errorf("%d requests still counted", ls.work.count())
	}
}

// SRV-7: work that does not stop when told makes the stop unclean, so the
// database is not closed under it.
func TestShutdownWithStuckWorkIsUnclean(t *testing.T) {
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	ls := startServer(t, Config{}, Deps{}, func(s *Server) {
		s.mux.HandleFunc("GET /test/stuck", func(w http.ResponseWriter, r *http.Request) {
			entered <- struct{}{}
			<-release
		})
	})
	defer close(release)
	go func() {
		if resp, err := http.Get(ls.url + "/test/stuck"); err == nil {
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
	h := openDB(t)
	s, err := New(Config{Limits: testLimits()}, Deps{DB: h.DB, Root: h.Root})
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

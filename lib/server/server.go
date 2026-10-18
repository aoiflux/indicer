// Package server is the DUES server: the Connect, gRPC and gRPC-Web service
// and the browser upload endpoints, on loopback by default (D3).
package server

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dgraph-io/badger/v4"
	"go.uber.org/zap"
)

// ErrUncleanShutdown means work was still running when the server stopped.
// The database must then be left as it is, for badger's recovery at the next
// open, rather than closed under that work.
var ErrUncleanShutdown = errors.New("work was still running when the server stopped; the database was left for recovery at its next open")

// errStopping is the cause of the request contexts the server cancels when
// it stops.
var errStopping = errors.New("the server is stopping")

// Config is what the server serves, where, and to whom.
type Config struct {
	// Listen holds host:port addresses. Empty serves 127.0.0.1 and [::1] on
	// DefaultPort.
	Listen []string
	// Token, when set, must be presented as a bearer token on every request
	// except health checks, on every listener. A non-loopback address needs
	// one.
	Token string
	// CORSOrigins are origins whose pages may call the server, besides
	// loopback ones, matched exactly (scheme://host[:port]).
	CORSOrigins []string
	Limits      Limits
}

// Limits bound what the server takes on.
type Limits struct {
	MaxConns          int
	MaxInFlight       int
	MaxJSONBody       int64
	ReadHeaderTimeout time.Duration
	IdleTimeout       time.Duration
	// PingInterval and PingTimeout keep idle HTTP/2 connections honest: the
	// server pings a connection silent this long, and drops it when the
	// ping goes unanswered.
	PingInterval, PingTimeout time.Duration
	// ShutdownGrace is how long requests may finish on their own when the
	// server stops; then their contexts are cancelled, and they have
	// IdleWait more to return.
	ShutdownGrace, IdleWait time.Duration
	Upload                  UploadLimits
}

// DefaultLimits are the limits `dues server` runs with.
func DefaultLimits() Limits {
	return Limits{
		MaxConns:          512,
		MaxInFlight:       256,
		MaxJSONBody:       64 << 10,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		PingInterval:      30 * time.Second,
		PingTimeout:       15 * time.Second,
		ShutdownGrace:     25 * time.Second,
		IdleWait:          10 * time.Second,
		Upload:            DefaultUploadLimits(),
	}
}

// Validate checks the configuration without listening: the addresses parse,
// a non-loopback address has a token, the token and origins are well formed.
func (c Config) Validate() error {
	addrs, err := parseListen(c.Listen)
	if err != nil {
		return err
	}
	if c.Token != "" {
		if err := checkToken(c.Token); err != nil {
			return err
		}
	}
	for _, a := range addrs {
		if !a.loopback && c.Token == "" {
			return fmt.Errorf("listen address %s is not a loopback address: serving it needs a token of at least %d characters (--token-file or DUES_TOKEN)", a.addr, MinTokenLength)
		}
	}
	for _, o := range c.CORSOrigins {
		if _, ok := normalizeOrigin(o); !ok {
			return fmt.Errorf("CORS origin %q: want scheme://host[:port] with scheme http or https", o)
		}
	}
	return nil
}

// Deps are what the server works on.
type Deps struct {
	DB *badger.DB
	// Root is the data root holding DB; uploads are kept under it.
	Root string
	Log  *zap.Logger
}

// Server serves one DUES database.
type Server struct {
	cfg      Config
	deps     Deps
	log      *zap.Logger
	addrs    []listenAddr
	tokenSum []byte // sha256 of the token, nil without one
	origins  map[string]bool

	inflight chan struct{}
	conns    chan struct{}
	work     tracker
	draining atomic.Bool

	base       context.Context
	cancelBase context.CancelCauseFunc

	uploads *uploads

	mux  *http.ServeMux
	http *http.Server
	lns  []net.Listener
	// Notes are things worth telling the operator, such as an optional
	// address that could not be served.
	Notes []string
}

// New builds a server for cfg; it listens only when Listen or Run is called.
func New(cfg Config, deps Deps) (*Server, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if deps.DB == nil {
		return nil, errors.New("server: no database")
	}
	if cfg.Limits == (Limits{}) {
		cfg.Limits = DefaultLimits()
	}
	if deps.Log == nil {
		deps.Log = zap.NewNop()
	}
	addrs, _ := parseListen(cfg.Listen)
	s := &Server{
		cfg:      cfg,
		deps:     deps,
		log:      deps.Log,
		addrs:    addrs,
		origins:  map[string]bool{},
		inflight: make(chan struct{}, max(cfg.Limits.MaxInFlight, 1)),
		conns:    make(chan struct{}, max(cfg.Limits.MaxConns, 1)),
		mux:      http.NewServeMux(),
	}
	if cfg.Token != "" {
		sum := sha256.Sum256([]byte(cfg.Token))
		s.tokenSum = sum[:]
	}
	for _, o := range cfg.CORSOrigins {
		n, _ := normalizeOrigin(o)
		s.origins[n] = true
	}
	s.base, s.cancelBase = context.WithCancelCause(context.Background())
	up, err := newUploads(s)
	if err != nil {
		return nil, err
	}
	s.uploads = up
	s.routes()
	s.http = s.newHTTPServer(s.handler())
	return s, nil
}

// handler is the middleware chain around the routes, outermost first:
// recover, access log, in-flight limit, Host check, CORS, auth. Body limits
// and deadlines are set per route.
func (s *Server) handler() http.Handler {
	var h http.Handler = s.mux
	h = s.withAuth(h)
	h = s.withCORS(h)
	h = s.withHostCheck(h)
	h = s.withInFlight(h)
	h = s.withAccessLog(h)
	return s.withRecover(h)
}

// routes registers every endpoint. Anything else, "/" included, is 404.
func (s *Server) routes() {
	health := route{read: 10 * time.Second, write: 10 * time.Second}
	s.handle(s.mux, "GET /healthz", health, http.HandlerFunc(s.healthz))
	s.handle(s.mux, "GET /readyz", health, http.HandlerFunc(s.readyz))
	s.rpcRoutes()
	s.uploadRoutes()
}

// newHTTPServer is the HTTP server for h. It speaks HTTP/1.1 and cleartext
// HTTP/2 with prior knowledge, which gRPC and Connect clients use on a
// plain-text listener.
func (s *Server) newHTTPServer(h http.Handler) *http.Server {
	var p http.Protocols
	p.SetHTTP1(true)
	p.SetUnencryptedHTTP2(true)
	lim := s.cfg.Limits
	return &http.Server{
		Handler:           h,
		Protocols:         &p,
		ReadHeaderTimeout: lim.ReadHeaderTimeout,
		IdleTimeout:       lim.IdleTimeout,
		MaxHeaderBytes:    64 << 10,
		HTTP2: &http.HTTP2Config{
			MaxConcurrentStreams: 100,
			SendPingTimeout:      lim.PingInterval,
			PingTimeout:          lim.PingTimeout,
		},
		BaseContext: func(net.Listener) context.Context { return s.base },
		ConnContext: func(ctx context.Context, c net.Conn) context.Context {
			return context.WithValue(ctx, loopbackConnKey, localIsLoopback(c))
		},
		ErrorLog: zap.NewStdLog(s.log.With(zap.String("component", "http"))),
	}
}

// Listen opens the listeners. An optional address that cannot be served is
// skipped with a note; any other failure closes what was opened.
func (s *Server) Listen() error {
	for _, a := range s.addrs {
		ln, err := net.Listen("tcp", a.addr)
		if err != nil {
			if a.optional {
				s.Notes = append(s.Notes, fmt.Sprintf("not listening on %s: %v", a.addr, err))
				continue
			}
			for _, l := range s.lns {
				_ = l.Close()
			}
			s.lns = nil
			return fmt.Errorf("listen on %s: %w", a.addr, err)
		}
		s.lns = append(s.lns, newLimitListener(ln, s.conns))
	}
	if len(s.lns) == 0 {
		return errors.New("the server has no address to listen on")
	}
	return nil
}

// Addrs are the addresses being listened on.
func (s *Server) Addrs() []net.Addr {
	out := make([]net.Addr, len(s.lns))
	for i, l := range s.lns {
		out[i] = l.Addr()
	}
	return out
}

// Run listens and serves until ctx ends.
func (s *Server) Run(ctx context.Context) error {
	if err := s.Listen(); err != nil {
		return err
	}
	return s.Serve(ctx)
}

// Serve serves on the listeners Listen opened until ctx ends or a listener
// fails, then stops the server (see stop). It returns nil after a clean stop.
func (s *Server) Serve(ctx context.Context) error {
	s.uploads.startReaper(s.base)
	serveErr := make(chan error, len(s.lns))
	for _, ln := range s.lns {
		go func() {
			err := s.http.Serve(ln)
			if errors.Is(err, http.ErrServerClosed) {
				err = nil
			}
			serveErr <- err
		}()
	}
	var failed error
	select {
	case <-ctx.Done():
		s.log.Info("server stopping", zap.String("cause", fmt.Sprint(context.Cause(ctx))))
	case failed = <-serveErr:
		if failed == nil {
			failed = errors.New("a listener stopped")
		}
		s.log.Error("server listener failed", zap.Error(failed))
	}
	return errors.Join(failed, s.stop())
}

// stop drains the server (SRV-7). New requests are refused; the requests
// being served get ShutdownGrace to finish; then their contexts, and that of
// any work they started, are cancelled, and they get IdleWait more. Work
// still running then is ErrUncleanShutdown.
func (s *Server) stop() error {
	s.draining.Store(true)
	lim := s.cfg.Limits
	sctx, cancel := context.WithTimeout(context.Background(), lim.ShutdownGrace)
	if err := s.http.Shutdown(sctx); err != nil {
		s.log.Warn("server requests still running after the grace period", zap.Error(err), zap.Int("running", s.work.count()))
	}
	cancel()
	// Requests and work still running are told to stop.
	s.cancelBase(errStopping)
	idle := s.work.wait(lim.IdleWait)
	_ = s.http.Close()
	s.stopUploads()
	if !idle {
		s.log.Error("server stopped with work still running", zap.Int("running", s.work.count()))
		return ErrUncleanShutdown
	}
	return nil
}

// goWork runs fn as server work: it counts towards a clean shutdown, and
// its context ends when the server stops.
func (s *Server) goWork(fn func(ctx context.Context)) {
	s.work.add()
	go func() {
		defer s.work.done()
		fn(s.base)
	}()
}

// tracker counts running requests and work, and waits for none.
type tracker struct {
	mu   sync.Mutex
	n    int
	zero chan struct{} // closed when n returns to 0
}

func (t *tracker) add() {
	t.mu.Lock()
	if t.n == 0 {
		t.zero = make(chan struct{})
	}
	t.n++
	t.mu.Unlock()
}

func (t *tracker) done() {
	t.mu.Lock()
	t.n--
	if t.n == 0 {
		close(t.zero)
	}
	t.mu.Unlock()
}

func (t *tracker) count() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.n
}

// wait reports whether the count reached zero within d.
func (t *tracker) wait(d time.Duration) bool {
	t.mu.Lock()
	if t.n == 0 {
		t.mu.Unlock()
		return true
	}
	ch := t.zero
	t.mu.Unlock()
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ch:
		return true
	case <-timer.C:
		return false
	}
}

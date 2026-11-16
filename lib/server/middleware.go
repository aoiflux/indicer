package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"runtime/debug"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

type ctxKey int

const (
	requestIDKey ctxKey = iota
	loopbackConnKey
	credentialKey
	authNoteKey
)

// RequestID returns the ID the server gave the request ctx belongs to.
func RequestID(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey).(string)
	return id
}

// statusWriter records the status and size of a response. It passes on
// Flush, and Unwrap lets http.ResponseController reach the connection.
type statusWriter struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (w *statusWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(p)
	w.bytes += int64(n)
	return n, err
}

func (w *statusWriter) Flush() {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	_ = http.NewResponseController(w.ResponseWriter).Flush()
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// errorBody is the JSON body of every error the HTTP routes return.
type errorBody struct {
	Error     string `json:"error"`
	RequestID string `json:"request_id,omitempty"`
}

// httpError writes a JSON error. Internal failures should pass a message that
// names no internal detail; the detail goes to the log.
func httpError(w http.ResponseWriter, r *http.Request, status int, msg string) {
	writeJSON(w, status, errorBody{Error: msg, RequestID: RequestID(r.Context())})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// decodeJSON reads a JSON body into v. A body over the route's limit is 413,
// a malformed one 400; both are answered, and false returned.
func decodeJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(r.Body)
	err := dec.Decode(v)
	if err == nil {
		// Nothing may follow the object.
		if _, terr := dec.Token(); terr != io.EOF {
			err = errors.New("unexpected data after the JSON object")
		}
	}
	var tooBig *http.MaxBytesError
	switch {
	case err == nil:
		return true
	case errors.As(err, &tooBig):
		httpError(w, r, http.StatusRequestEntityTooLarge, fmt.Sprintf("request body is larger than %d bytes", tooBig.Limit))
	default:
		httpError(w, r, http.StatusBadRequest, "invalid request body: "+err.Error())
	}
	return false
}

// withRecover gives every request its ID and turns a handler panic into a
// logged 500. It is the outermost layer, so a panic anywhere below is caught.
func (s *Server) withRecover(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := newRequestID()
		w.Header().Set("Request-Id", id)
		r = r.WithContext(context.WithValue(r.Context(), requestIDKey, id))
		sw := &statusWriter{ResponseWriter: w}
		defer func() {
			v := recover()
			if v == nil {
				return
			}
			if v == http.ErrAbortHandler {
				panic(v)
			}
			stack := debug.Stack()
			if len(stack) > 64<<10 {
				stack = stack[:64<<10]
			}
			s.log.Error("server PANIC", zap.String("request_id", id), zap.String("path", r.URL.Path),
				zap.String("panic", fmt.Sprint(v)), zap.ByteString("stack", stack))
			if sw.status == 0 {
				httpError(sw, r, http.StatusInternalServerError, fmt.Sprintf("internal error (request %s)", id))
			}
		}()
		next.ServeHTTP(sw, r)
	})
}

func newRequestID() string {
	if id, err := uuid.NewV7(); err == nil {
		return id.String()
	}
	return uuid.NewString()
}

// withAccessLog logs one line per request, with the label of the token it
// came with. No header is logged, so the Authorization header never reaches
// the log.
func (s *Server) withAccessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw, ok := w.(*statusWriter)
		if !ok {
			sw = &statusWriter{ResponseWriter: w}
		}
		note := &authNote{}
		next.ServeHTTP(sw, r.WithContext(context.WithValue(r.Context(), authNoteKey, note)))
		status := sw.status
		if status == 0 {
			status = http.StatusOK
		}
		s.log.Info("server request",
			zap.String("request_id", RequestID(r.Context())),
			zap.String("method", r.Method),
			zap.String("path", truncate(r.URL.Path, 256)),
			zap.String("proto", r.Proto),
			zap.String("remote", r.RemoteAddr),
			bearerField(note.label),
			zap.Int("status", status),
			zap.Int64("bytes", sw.bytes),
			zap.Int64("duration_ms", time.Since(start).Milliseconds()))
	})
}

// bearerField names a credential by its label; there is none to name when
// no token is set.
func bearerField(label string) zap.Field {
	if label == "" {
		return zap.Skip()
	}
	return zap.String(logFieldBearer, label)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// withInFlight bounds the requests being served at once, and refuses new
// requests once the server is draining. Health checks are counted too: a
// server too busy to answer is not ready.
func (s *Server) withInFlight(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.draining.Load() && !isHealthPath(r.URL.Path) {
			w.Header().Set("Connection", "close")
			httpError(w, r, http.StatusServiceUnavailable, "the server is shutting down")
			return
		}
		select {
		case s.inflight <- struct{}{}:
		default:
			w.Header().Set(headerRetryAfter, busyRetryAfter)
			httpError(w, r, http.StatusServiceUnavailable, "the server is busy; retry shortly")
			return
		}
		s.work.add()
		defer func() {
			s.work.done()
			<-s.inflight
		}()
		next.ServeHTTP(w, r)
	})
}

// withHostCheck refuses a request that arrived on a loopback address but
// names another host: a page whose name was rebound to 127.0.0.1 (DNS
// rebinding) sends its own name, and is answered 421.
func (s *Server) withHostCheck(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if lb, _ := r.Context().Value(loopbackConnKey).(bool); lb && r.Host != "" && !isLoopbackHost(hostOfHostport(r.Host)) {
			httpError(w, r, http.StatusMisdirectedRequest, "this server answers only to localhost names on a loopback address")
			return
		}
		next.ServeHTTP(w, r)
	})
}

const (
	corsAllowMethods = "GET, POST, OPTIONS"
	corsAllowHeaders = "Authorization, Content-Type, Connect-Protocol-Version, Connect-Timeout-Ms, " +
		"Connect-Accept-Encoding, Connect-Content-Encoding, Grpc-Timeout, X-Grpc-Web, X-User-Agent, " +
		"Upload-Id, Upload-Offset"
	corsExposeHeaders = "Grpc-Status, Grpc-Message, Grpc-Status-Details-Bin, Request-Id, Upload-Offset"
)

// withCORS lets browser pages from loopback origins, on any port, and from
// the configured origins call the server. A request from any other origin is
// refused, preflight or not, so a foreign page cannot make the browser send
// it even a "simple" request (CSRF). Requests without an Origin header are
// not from a browser page and pass.
func (s *Server) withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Add("Vary", "Origin")
		origin := r.Header.Get("Origin")
		if origin == "" {
			next.ServeHTTP(w, r)
			return
		}
		if !s.originAllowed(origin) {
			s.log.Warn("server refused origin", zap.String("request_id", RequestID(r.Context())), zap.String("origin", truncate(origin, 256)))
			httpError(w, r, http.StatusForbidden, "origin not allowed")
			return
		}
		h.Set("Access-Control-Allow-Origin", origin)
		if r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != "" {
			h.Add("Vary", "Access-Control-Request-Method")
			h.Add("Vary", "Access-Control-Request-Headers")
			h.Set("Access-Control-Allow-Methods", corsAllowMethods)
			h.Set("Access-Control-Allow-Headers", corsAllowHeaders)
			h.Set("Access-Control-Max-Age", "600")
			w.WriteHeader(http.StatusNoContent)
			return
		}
		h.Set("Access-Control-Expose-Headers", corsExposeHeaders)
		next.ServeHTTP(w, r)
	})
}

// originAllowed reports whether a page from origin may call the server.
func (s *Server) originAllowed(origin string) bool {
	o, ok := normalizeOrigin(origin)
	if !ok {
		return false
	}
	if s.origins[o] {
		return true
	}
	_, host, _ := strings.Cut(o, "://")
	return isLoopbackHost(hostOfHostport(host))
}

// withAuth finds the route a request is for, and lets the request in when
// the route is public, or its token holds the route's scope. A route
// registered without a scope is refused, so none is served by mistake. A
// request that matches no route gets the mux's 404 or 405, once
// authenticated. CORS preflights were answered before this layer: a browser
// sends them without credentials.
func (s *Server) withAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, pattern := s.mux.Handler(r)
		rt, known := s.registered[pattern]
		if known && rt.public {
			next.ServeHTTP(w, r)
			return
		}
		cred, ok := s.authenticate(r.Header.Get("Authorization"))
		if !ok {
			w.Header().Set("WWW-Authenticate", bearerChallenge)
			httpError(w, r, http.StatusUnauthorized, "a valid bearer token is required")
			return
		}
		noteCredential(r.Context(), cred)
		switch {
		case pattern == "":
		case !known:
			s.log.Warn("server route has no scope", zap.String("request_id", RequestID(r.Context())), zap.String("pattern", pattern))
			httpError(w, r, http.StatusForbidden, "this route is not served")
			return
		case cred.scopes&rt.scope != rt.scope:
			s.refuseScope(w, r, rt, cred)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), credentialKey, cred)))
	})
}

// noTokenScope is the refusal of a scope the server cannot grant, because it
// has no token: export, which is never anonymous.
const noTokenScope = "%s needs a token with that scope; start the server with --token-file naming a file that begins %q and gives a token %s or %s"

// bearerChallenge is the WWW-Authenticate challenge of a refused request.
const bearerChallenge = `Bearer realm="dues"`

// refuseScope answers 403 to a token without the route's scope. An RPC
// route answers with a Connect error, which every RPC client reads; the JSON
// body of the other routes would reach a streaming or gRPC client as a bare
// status.
func (s *Server) refuseScope(w http.ResponseWriter, r *http.Request, rt route, cred Credential) {
	need := rt.scope &^ cred.scopes
	s.log.Warn("server scope refused", zap.String("request_id", RequestID(r.Context())),
		zap.String("path", truncate(r.URL.Path, 256)), bearerField(cred.label), zap.String("scope", need.String()))
	var msg string
	if len(s.cfg.Tokens) == 0 {
		msg = fmt.Sprintf(noTokenScope, need, TokenFileHeader, need, scopeAllName)
	} else {
		w.Header().Set("WWW-Authenticate", fmt.Sprintf(`%s, error="insufficient_scope", scope="%s"`, bearerChallenge, strings.Join(need.names(), " ")))
		msg = fmt.Sprintf("this token may not %s: its scopes are %s", need, cred.scopes)
	}
	if rt.rpc {
		_ = s.rpcErr.Write(w, r, connect.NewError(connect.CodePermissionDenied, errors.New(msg)))
		return
	}
	httpError(w, r, http.StatusForbidden, msg)
}

func isHealthPath(p string) bool { return p == "/healthz" || p == "/readyz" }

// route says how a route's request is bounded.
type route struct {
	// maxBody caps the request body; 0 leaves it to the handler.
	maxBody int64
	// read is the time allowed to read the whole request; idleRead, when
	// set, instead allows that long between reads of the body, for bodies
	// that may take long but must keep moving.
	read, idleRead time.Duration
	// write is the time allowed for the response, counted from the start of
	// the request; 0 leaves it unbounded (the request context still ends at
	// shutdown).
	write time.Duration
	// scope is what a token needs to call the route; a public route needs no
	// token. A route is one or the other.
	scope  Scope
	public bool
	// rpc routes are answered by Connect handlers.
	rpc bool
	// lease routes use the database: each request holds a lease on it, and
	// is refused while it is paused. An RPC's lease ends when its handler
	// returns, before the response is written. A public route takes none.
	lease bool
}

// handle registers a route on the server's mux.
func (s *Server) handle(pattern string, rt route, h http.Handler) {
	if rt.public == (rt.scope != 0) {
		panic(fmt.Sprintf("server: route %q must be either public or scoped", pattern))
	}
	if rt.lease && rt.public {
		panic(fmt.Sprintf("server: route %q uses the database, so it must be scoped", pattern))
	}
	s.registered[pattern] = rt
	s.mux.Handle(pattern, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rc := http.NewResponseController(w)
		now := time.Now()
		if rt.idleRead > 0 {
			_ = rc.SetReadDeadline(now.Add(rt.idleRead))
			r.Body = &idleBody{ReadCloser: r.Body, rc: rc, idle: rt.idleRead}
		} else if rt.read > 0 {
			_ = rc.SetReadDeadline(now.Add(rt.read))
		}
		if rt.write > 0 {
			_ = rc.SetWriteDeadline(now.Add(rt.write))
		}
		if rt.maxBody > 0 {
			r.Body = http.MaxBytesReader(w, r.Body, rt.maxBody)
		}
		if rt.lease {
			ctx, end, ok := s.dbs.lease(r.Context())
			if !ok {
				s.refusePaused(w, r, rt)
				return
			}
			defer end()
			r = r.WithContext(ctx)
		}
		h.ServeHTTP(w, r)
	}))
}

// idleBody moves the read deadline on before every read, so a body that
// keeps arriving is never cut off, and one that stalls is. Once the body
// ends it clears the deadline: over HTTP/1.1 the server goes on reading the
// connection, and a deadline hit there cancels the request, whose upload may
// still be waiting through a pause to be stored.
type idleBody struct {
	io.ReadCloser
	rc   *http.ResponseController
	idle time.Duration
}

func (b *idleBody) Read(p []byte) (int, error) {
	_ = b.rc.SetReadDeadline(time.Now().Add(b.idle))
	n, err := b.ReadCloser.Read(p)
	if errors.Is(err, io.EOF) {
		_ = b.rc.SetReadDeadline(time.Time{})
	}
	return n, err
}

// healthz answers that the process is serving.
func (s *Server) healthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set(headerCacheControl, cacheNoStore)
	_, _ = io.WriteString(w, "ok\n")
}

// readyz answers whether the server takes work: not while it drains, not
// while its database is paused, which says when to ask again, and not once
// its database is closed.
func (s *Server) readyz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set(headerCacheControl, cacheNoStore)
	status, body := http.StatusOK, bodyReady
	switch ref := s.dbs.current(); {
	case s.draining.Load():
		status, body = http.StatusServiceUnavailable, bodyNotReady
	case s.dbs.isPaused():
		w.Header().Set(headerRetryAfter, pausedRetryAfter)
		status, body = http.StatusServiceUnavailable, bodyPaused
	case ref == nil || ref.db.IsClosed():
		status, body = http.StatusServiceUnavailable, bodyNotReady
	}
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}

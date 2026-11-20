package server

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"sync"
)

// DefaultPort is the port the server listens on when no address is given.
const DefaultPort = "50051"

// MinTokenLength is the shortest bearer token the server accepts.
const MinTokenLength = 32

// listenAddr is one address to serve on.
type listenAddr struct {
	addr     string
	loopback bool
	// optional: failing to listen here is a note, not an error. The default
	// IPv6 loopback is optional, since a host may have no IPv6 loopback.
	optional bool
}

// defaultAddrs are the loopback addresses served when none is given (D3).
func defaultAddrs() []listenAddr {
	return []listenAddr{
		{addr: net.JoinHostPort("127.0.0.1", DefaultPort), loopback: true},
		{addr: net.JoinHostPort("::1", DefaultPort), loopback: true, optional: true},
	}
}

// parseListen checks the --listen addresses and says which are loopback.
func parseListen(list []string) ([]listenAddr, error) {
	if len(list) == 0 {
		return defaultAddrs(), nil
	}
	seen := map[string]bool{}
	out := make([]listenAddr, 0, len(list))
	for _, a := range list {
		host, port, err := net.SplitHostPort(strings.TrimSpace(a))
		if err != nil {
			return nil, fmt.Errorf("listen address %q: want host:port (%v)", a, err)
		}
		p, err := strconv.Atoi(port)
		if err != nil || p < 0 || p > 65535 {
			return nil, fmt.Errorf("listen address %q: port must be 0 to 65535", a)
		}
		addr := net.JoinHostPort(host, port)
		if seen[addr] {
			return nil, fmt.Errorf("listen address %q is given twice", a)
		}
		seen[addr] = true
		out = append(out, listenAddr{addr: addr, loopback: isLoopbackHost(host)})
	}
	return out, nil
}

// IsLoopbackAddr reports whether the host:port address addr is served only
// on the loopback interface.
func IsLoopbackAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	return err == nil && isLoopbackHost(host)
}

// isLoopbackHost reports whether host names only the loopback interface:
// "localhost", a name under it, or a loopback IP. An empty host (all
// interfaces) and every other name are not loopback; a name is not resolved.
func isLoopbackHost(host string) bool {
	h := strings.ToLower(strings.TrimSuffix(strings.TrimSuffix(strings.TrimPrefix(host, "["), "]"), "."))
	if h == "localhost" || strings.HasSuffix(h, ".localhost") {
		return true
	}
	ip, err := netip.ParseAddr(h)
	return err == nil && ip.Unmap().IsLoopback()
}

// hostOfHostport returns the host part of a Host header value, which may or
// may not carry a port.
func hostOfHostport(hp string) string {
	if h, _, err := net.SplitHostPort(hp); err == nil {
		return h
	}
	return hp
}

// checkToken checks a bearer token: long enough, and only the visible ASCII
// an Authorization header can carry.
func checkToken(tok string) error {
	if len(tok) < MinTokenLength {
		return fmt.Errorf("the token must be at least %d characters (it has %d)", MinTokenLength, len(tok))
	}
	for i := 0; i < len(tok); i++ {
		if c := tok[i]; c < 0x21 || c > 0x7e {
			return errors.New("the token must be visible ASCII, with no spaces")
		}
	}
	return nil
}

// normalizeOrigin returns an origin in the form browsers send it,
// scheme://host[:port] with the scheme and host in lower case and a default
// port left out, or false when s is not an http(s) origin.
func normalizeOrigin(s string) (string, bool) {
	u, err := url.Parse(s)
	if err != nil || u.Opaque != "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", false
	}
	if u.Path != "" && u.Path != "/" {
		return "", false
	}
	scheme := strings.ToLower(u.Scheme)
	if (scheme != "http" && scheme != "https") || u.Hostname() == "" {
		return "", false
	}
	host := strings.ToLower(u.Hostname())
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	switch port := u.Port(); {
	case port == "", scheme == "http" && port == "80", scheme == "https" && port == "443":
	default:
		host += ":" + port
	}
	return scheme + "://" + host, true
}

// limitListener accepts at most cap(sem) connections at once, shared by every
// listener holding the same semaphore; further connections wait in the
// kernel's backlog until one closes.
type limitListener struct {
	net.Listener
	sem  chan struct{}
	done chan struct{}
	once sync.Once
}

func newLimitListener(ln net.Listener, sem chan struct{}) *limitListener {
	return &limitListener{Listener: ln, sem: sem, done: make(chan struct{})}
}

func (l *limitListener) Accept() (net.Conn, error) {
	select {
	case l.sem <- struct{}{}:
	case <-l.done:
		return nil, net.ErrClosed
	}
	c, err := l.Listener.Accept()
	if err != nil {
		<-l.sem
		return nil, err
	}
	return &limitConn{Conn: c, release: func() { <-l.sem }}, nil
}

func (l *limitListener) Close() error {
	l.once.Do(func() { close(l.done) })
	return l.Listener.Close()
}

type limitConn struct {
	net.Conn
	once    sync.Once
	release func()
}

func (c *limitConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(c.release)
	return err
}

// localIsLoopback reports whether a connection arrived on a loopback address.
func localIsLoopback(c net.Conn) bool {
	if lc, ok := c.(*limitConn); ok {
		c = lc.Conn
	}
	ta, ok := c.LocalAddr().(*net.TCPAddr)
	return ok && ta.IP.IsLoopback()
}

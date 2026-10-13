package api

import (
	"io"
	"net"
	"net/http"
	"testing"
)

// TestHTTPServerProtocols checks that the server speaks HTTP/1.1 and
// cleartext HTTP/2 with prior knowledge, the transport gRPC and Connect
// clients use against a plain-text listener.
func TestHTTPServerProtocols(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := newHTTPServer(ln.Addr().String(), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, r.Proto)
	}))
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

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
			resp, err := (&http.Client{Transport: tr}).Get("http://" + ln.Addr().String() + "/")
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

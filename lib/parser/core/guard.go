package core

import (
	"context"
	"io"

	"indicer/internal/safe"
)

// Guard returns fs with every call made through internal/safe, so a panic in
// the backing library becomes an error on that one call: a node's Stat or
// Content fails, not the scan. A panic during Walk ends the walk with an
// error; the nodes already visited stand.
func Guard(fs Filesystem) Filesystem {
	if g, ok := fs.(*guarded); ok {
		return g
	}
	return &guarded{fs: fs, comp: "parser/" + string(fs.Type())}
}

type guarded struct {
	fs   Filesystem
	comp string
}

func (g *guarded) site(op string, n Node) safe.Site {
	return safe.Site{Component: g.comp, Op: op, FSType: string(g.fs.Type()), Path: n.Path, Inode: n.ID}
}

func (g *guarded) Type() FSType               { return g.fs.Type() }
func (g *guarded) Capabilities() Capabilities { return g.fs.Capabilities() }

func (g *guarded) Walk(ctx context.Context, fn func(Node) error) error {
	var cbErr error
	err := safe.Call(g.site("walk", Node{}), func() error {
		return g.fs.Walk(ctx, func(n Node) error {
			// A panic inside fn is the caller's, not the library's; keep it
			// apart so it is not reported as a filesystem fault.
			if e := fn(n); e != nil {
				cbErr = e
				return e
			}
			return nil
		})
	})
	if cbErr != nil {
		return cbErr
	}
	return err
}

func (g *guarded) Stat(n Node) (FileInfo, error) {
	return safe.Value(g.site("stat", n), func() (FileInfo, error) { return g.fs.Stat(n) })
}

func (g *guarded) Content(n Node) (Content, error) {
	return safe.Value(g.site("content", n), func() (Content, error) { return g.fs.Content(n) })
}

func (g *guarded) Streams(n Node) ([]Stream, error) {
	return safe.Value(g.site("streams", n), func() ([]Stream, error) { return g.fs.Streams(n) })
}

func (g *guarded) Open(n Node) (io.ReadCloser, error) {
	rc, err := safe.Value(g.site("open", n), func() (io.ReadCloser, error) { return g.fs.Open(n) })
	if err != nil {
		return nil, err
	}
	return &guardedReader{rc: rc, site: g.site("read", n)}, nil
}

func (g *guarded) Close() error {
	return safe.Call(g.site("close", Node{}), g.fs.Close)
}

// guardedReader carries the guard into reads: libraries decode lazily, so a
// panic can come from Read long after Open returned.
type guardedReader struct {
	rc   io.ReadCloser
	site safe.Site
}

func (r *guardedReader) Read(p []byte) (int, error) {
	type res struct{ n int }
	out, err := safe.Value(r.site, func() (res, error) {
		n, err := r.rc.Read(p)
		return res{n}, err
	})
	return out.n, err
}

func (r *guardedReader) Close() error { return safe.Call(r.site, r.rc.Close) }

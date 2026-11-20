package core

import (
	"context"
	"errors"
	"io"
	"testing"

	"indicer/internal/safe"
)

type panicFS struct{ walkPanicAt int }

func (panicFS) Type() FSType               { return FSExt }
func (panicFS) Capabilities() Capabilities { return Capabilities{} }
func (f panicFS) Walk(_ context.Context, fn func(Node) error) error {
	for i := 0; i < 5; i++ {
		if i == f.walkPanicAt {
			panic("corrupt directory")
		}
		if err := fn(Node{ID: uint64(i), Path: "/f"}); err != nil {
			return err
		}
	}
	return nil
}
func (panicFS) Stat(Node) (FileInfo, error) { panic("bad inode") }
func (panicFS) Content(Node) (Content, error) {
	var ext []Extent
	return Content{Extents: ext[:1]}, nil // a library misreading its input
}
func (panicFS) Streams(Node) ([]Stream, error)   { return nil, nil }
func (panicFS) Open(Node) (io.ReadCloser, error) { return io.NopCloser(panicReader{}), nil }
func (panicFS) Close() error                     { return nil }

type panicReader struct{}

func (panicReader) Read([]byte) (int, error) { panic("lazy decode") }

func TestGuardContainsPanics(t *testing.T) {
	t.Parallel()
	g := Guard(panicFS{walkPanicAt: 3})
	if _, err := g.Stat(Node{}); !safe.IsPanic(err) {
		t.Fatalf("Stat: %v, want a recovered panic", err)
	}
	if _, err := g.Content(Node{}); !safe.IsPanic(err) {
		t.Fatalf("Content: %v, want a recovered panic", err)
	}
	rc, err := g.Open(Node{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadAll(rc); !safe.IsPanic(err) {
		t.Fatalf("Read: %v, want a recovered panic", err)
	}

	var seen int
	err = g.Walk(context.Background(), func(Node) error { seen++; return nil })
	if !safe.IsPanic(err) || seen != 3 {
		t.Fatalf("Walk: err %v after %d nodes, want a panic after 3", err, seen)
	}
}

func TestGuardKeepsCallbackErrors(t *testing.T) {
	t.Parallel()
	stop := errors.New("stop")
	err := Guard(panicFS{walkPanicAt: -1}).Walk(context.Background(), func(Node) error { return stop })
	if err != stop {
		t.Fatalf("Walk: %v, want the callback's error unchanged", err)
	}
}

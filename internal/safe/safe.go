// Package safe contains panics.
//
// Parsers for evidence formats run on attacker-controlled input, and a panic
// in any goroutine ends the whole process: an ingest dies half-written and a
// server drops every client. Code that calls third-party parsers, or runs work
// on its own goroutine, goes through this package. A panic becomes a
// *PanicError carrying where it happened, and the caller decides what one
// failed file or partition means.
package safe

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
)

// Site says where a panic happened. Only Component and Op are required; the
// rest is filled in when the caller knows it.
type Site struct {
	Component string // e.g. "parser/ntfs", "ingest"
	Op        string // e.g. "walk", "stat", "content"
	Evidence  string
	FSType    string
	Partition string
	Path      string
	Inode     uint64
	Offset    int64
}

func (s Site) key() string { return s.Component + "/" + s.Op }

func (s Site) String() string {
	var b strings.Builder
	b.WriteString(s.key())
	add := func(k, v string) {
		if v != "" {
			fmt.Fprintf(&b, " %s=%q", k, v)
		}
	}
	add("evidence", s.Evidence)
	add("fs", s.FSType)
	add("partition", s.Partition)
	add("path", s.Path)
	if s.Inode != 0 {
		fmt.Fprintf(&b, " inode=%d", s.Inode)
	}
	if s.Offset != 0 {
		fmt.Fprintf(&b, " offset=%d", s.Offset)
	}
	return b.String()
}

// maxStack bounds the stack kept with a PanicError. A deep recursion panic can
// produce megabytes of identical frames.
const maxStack = 64 << 10

// PanicError is a recovered panic.
type PanicError struct {
	Site  Site
	Value any
	Stack []byte
	ID    string // short random id, printed to the user and written to the log
}

func (e *PanicError) Error() string {
	return fmt.Sprintf("internal error in %s (panic %s): %v", e.Site, e.ID, e.Value)
}

// Unwrap exposes a panic value that was itself an error.
func (e *PanicError) Unwrap() error {
	if err, ok := e.Value.(error); ok {
		return err
	}
	return nil
}

// Reporter receives each recovered panic. first is true for the first panic at
// a Site's component and op; later ones at the same place are only counted, so
// a corrupt volume cannot flood the log with a million identical stacks.
type Reporter func(e *PanicError, first bool)

var (
	mu       sync.Mutex
	reporter Reporter
	counts   = map[string]int{}
)

// SetReporter installs the function told about recovered panics, typically
// one that logs them. It returns the previous reporter.
func SetReporter(r Reporter) Reporter {
	mu.Lock()
	defer mu.Unlock()
	prev := reporter
	reporter = r
	return prev
}

// Counts returns how many panics have been recovered at each component/op.
func Counts() map[string]int {
	mu.Lock()
	defer mu.Unlock()
	out := make(map[string]int, len(counts))
	for k, v := range counts {
		out[k] = v
	}
	return out
}

// strictMode makes recovered panics re-panic, so fuzzing sees the original
// crash instead of a handled error. It starts as the safestrict build tag
// says (strictDefault); there is no run-time switch.
var strictMode atomic.Bool

func init() { strictMode.Store(strictDefault) }

// SetStrict turns strict mode on or off for a test and returns the previous
// setting.
func SetStrict(on bool) (prev bool) { return strictMode.Swap(on) }

func newID() string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// recovered turns a recover() value into a *PanicError and reports it.
func recovered(site Site, v any) *PanicError {
	stack := debug.Stack()
	if len(stack) > maxStack {
		stack = stack[:maxStack]
	}
	e := &PanicError{Site: site, Value: v, Stack: stack, ID: newID()}

	mu.Lock()
	counts[site.key()]++
	first := counts[site.key()] == 1
	r := reporter
	mu.Unlock()

	if r != nil {
		r(e, first)
	}
	if strictMode.Load() {
		panic(fmt.Sprintf("%v\n\noriginal stack:\n%s", e, e.Stack))
	}
	return e
}

// Call runs fn and returns its error, or a *PanicError if it panicked.
func Call(site Site, fn func() error) (err error) {
	defer func() {
		if v := recover(); v != nil {
			err = recovered(site, v)
		}
	}()
	return fn()
}

// Value runs fn and returns its results, or the zero value and a *PanicError
// if it panicked.
func Value[T any](site Site, fn func() (T, error)) (out T, err error) {
	defer func() {
		if v := recover(); v != nil {
			var zero T
			out, err = zero, recovered(site, v)
		}
	}()
	return fn()
}

// Go runs fn on a new goroutine. The returned channel receives fn's error, or
// a *PanicError, exactly once and is then closed.
func Go(site Site, fn func() error) <-chan error {
	ch := make(chan error, 1)
	go func() {
		defer close(ch)
		ch <- Call(site, fn)
	}()
	return ch
}

// IsPanic reports whether err is, or wraps, a recovered panic.
func IsPanic(err error) bool {
	var pe *PanicError
	return errors.As(err, &pe)
}

// Group runs goroutines that share a context, like errgroup.Group, but a panic
// in one becomes that goroutine's error instead of ending the process. The
// first error cancels the context.
type Group struct {
	wg     sync.WaitGroup
	cancel context.CancelCauseFunc
	sem    chan struct{}

	once sync.Once
	err  error
}

// WithContext returns a Group and a context that is cancelled when a
// goroutine fails or Wait returns.
func WithContext(ctx context.Context) (*Group, context.Context) {
	ctx, cancel := context.WithCancelCause(ctx)
	return &Group{cancel: cancel}, ctx
}

// SetLimit caps how many goroutines run at once. Call it before Go.
func (g *Group) SetLimit(n int) {
	if n > 0 {
		g.sem = make(chan struct{}, n)
	}
}

// Go runs fn in a goroutine, first blocking while the limit is reached.
func (g *Group) Go(site Site, fn func() error) {
	if g.sem != nil {
		g.sem <- struct{}{}
	}
	g.wg.Add(1)
	go func() {
		defer g.wg.Done()
		if g.sem != nil {
			defer func() { <-g.sem }()
		}
		if err := Call(site, fn); err != nil {
			g.once.Do(func() {
				g.err = err
				if g.cancel != nil {
					g.cancel(err)
				}
			})
		}
	}()
}

// Wait blocks until every goroutine has returned, then returns the first
// error. No goroutine started by the Group outlives Wait.
func (g *Group) Wait() error {
	g.wg.Wait()
	if g.cancel != nil {
		g.cancel(nil)
	}
	return g.err
}

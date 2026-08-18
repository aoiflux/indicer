package graph

import (
	"fmt"

	graphstore "github.com/aoiflux/graphene/store"
)

// ActorRef identifies who is responsible for a write.
//
// The IDs are derived rather than registered, so offline forensic workstations
// do not need to coordinate. graphene reserves actor 0 for "unattributed", so a
// derivation that lands on 0 is remapped.
//
// P4 populates this from the signer provider; until then a caller may supply it
// directly, and the zero value writes unattributed — which is what DUES did for
// every write before transactions existed.
type ActorRef struct {
	Name    string
	ActorID uint64
	RoleID  uint32
	KeyID   uint64
}

// Unattributed reports whether this reference names nobody.
func (a ActorRef) Unattributed() bool { return a.ActorID == 0 }

// TxContext renders the reference in the form graphene records.
func (a ActorRef) TxContext() graphstore.TxContext {
	return graphstore.TxContext{ActorID: a.ActorID, RoleID: a.RoleID, KeyID: a.KeyID}
}

// Writer is an exclusive write session over a handle.
//
// One writer exists at a time per handle. That is not a performance choice: the
// upsert DUES performs — look a node up by an indexed property, insert if
// absent — is two graphene calls, and graphene can only make each one atomic on
// its own. Two goroutines running the pair concurrently both miss and both
// insert.
//
// Close must be called; the usual shape is defer.
type Writer struct {
	handle   *Handle
	actor    ActorRef
	producer Producer
	closed   bool
}

// Writer acquires the exclusive write session. It blocks until any current
// writer closes.
func (h *Handle) Writer(actor ActorRef) (*Writer, error) {
	if h == nil {
		return nil, fmt.Errorf("graph: Writer on a nil handle")
	}
	if h.readOnly {
		return nil, fmt.Errorf("graph: %s is open read-only; writes are refused", h.key)
	}
	h.writeMu.Lock()
	return &Writer{handle: h, actor: actor, producer: DefaultProducer}, nil
}

// As names the subsystem and version deriving these writes. Every node and edge
// the writer's batches create is stamped with it.
//
// A writer that names no producer still stamps DefaultProducer, so derived data
// is never written unattributed — see the Producer doc for why that matters.
func (w *Writer) As(producer Producer) *Writer {
	if w != nil && !producer.Zero() {
		w.producer = producer
	}
	return w
}

// Producer is the stamp applied to this writer's output.
func (w *Writer) Producer() Producer { return w.producer }

// Close releases the write session.
func (w *Writer) Close() {
	if w == nil || w.closed {
		return
	}
	w.closed = true
	w.handle.writeMu.Unlock()
}

// Actor is the reference every batch from this writer is attributed to.
func (w *Writer) Actor() ActorRef { return w.actor }

// Handle exposes the store this writer belongs to, for reads taken alongside
// the writes.
func (w *Writer) Handle() *Handle { return w.handle }

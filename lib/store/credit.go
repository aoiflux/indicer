package store

import (
	"sync"
	"sync/atomic"
)

// Passes that run at once and share a chunk count it new for whichever
// stores it first, so what each counts depends on timing: two evidence of a
// folder stored side by side that share a run of zeros, or an evidence and an
// object its index stores beside it. ChunkCredits makes the passes of a run
// count as they would one at a time, in order: a chunk is new for the first
// pass in that order that needs it. The order is the run's items, and within
// an item its evidence's own pass comes before the passes of the objects its
// index stores.
//
// The ledger holds each chunk stored in the run that a pass before its owner
// may still need: a pass of an earlier item, which has not been settled, or
// the item's own evidence pass, while it runs. A pass that needs a chunk a
// pass after it owns takes the chunk over, and the chunk's bytes move from
// the owner's item to its own (Adjust). Only an evidence's own pass counts
// its bytes (ChunkResult), so a chunk moving to or from the pass of an object
// moves them one way only.
type ChunkCredits struct {
	// next is the first item not settled: no pass of an item before it runs.
	next atomic.Int64
	// held is len(owners), for the checks made without the lock.
	held atomic.Int64
	// locked counts the stored and found calls that took the lock.
	locked atomic.Int64

	mu     sync.Mutex
	owners map[string]chunkOwner
	// byItem lists the chunks each item came to own, for letting go of them;
	// a chunk taken over since stays listed under the item it left.
	byItem map[int][]string
	moved  map[int]movedBytes
	// ended are the items whose evidence pass ended.
	ended map[int]bool
}

// creditsHeld is how many chunks a ledger holds before it is Full: a few MiB.
const creditsHeld = 1 << 16

// rank is a pass's place in its run.
type rank struct {
	item   int
	object bool
}

// before reports whether a pass of r comes before one of o.
func (r rank) before(o rank) bool {
	return r.item < o.item || r.item == o.item && !r.object && o.object
}

type chunkOwner struct {
	rank
	n, written int64
}

type movedBytes struct{ new, written int64 }

// Credit places a chunk pass in a run whose passes count new chunks as one
// pass at a time would (ChunkCredits). The zero Credit is in no run: its pass
// counts what it stores.
type Credit struct {
	Ledger *ChunkCredits
	// Item is the pass's place in the run.
	Item int
	// Object marks the pass of an object the item's index stores, which
	// comes after the item's evidence pass.
	Object bool
}

// Objects is the Credit of the passes of the objects c's item's index stores.
func (c Credit) Objects() Credit {
	c.Object = true
	return c
}

func (c Credit) rank() rank { return rank{item: c.Item, object: c.Object} }

// NewChunkCredits starts the ledger of one run.
func NewChunkCredits() *ChunkCredits {
	return &ChunkCredits{owners: map[string]chunkOwner{}, byItem: map[int][]string{}, moved: map[int]movedBytes{}, ended: map[int]bool{}}
}

// stored records that a pass of c stored the chunk key, n bytes written as
// written, before any other pass can find it. An owner the chunk had failed
// to store it (its claim was dropped, or its pass never flushed), so the
// chunk is c's now.
func (l *ChunkCredits) stored(key []byte, c Credit, n, written int64) {
	if l == nil || !c.Object && int64(c.Item) <= l.next.Load() && l.held.Load() == 0 {
		// No pass before it runs, and no chunk is held.
		return
	}
	l.locked.Add(1)
	l.mu.Lock()
	defer l.mu.Unlock()
	if r := c.rank(); l.needed(r) {
		l.own(string(key), chunkOwner{rank: r, n: n, written: written})
	} else {
		delete(l.owners, string(key))
	}
	l.held.Store(int64(len(l.owners)))
}

// found records that a pass of c needs the chunk key, which another pass
// stored, and takes the chunk over when that pass comes after it.
func (l *ChunkCredits) found(key []byte, c Credit) {
	if l == nil || l.held.Load() == 0 {
		return
	}
	l.locked.Add(1)
	l.mu.Lock()
	defer l.mu.Unlock()
	o, ok := l.owners[string(key)]
	r := c.rank()
	if !ok || !r.before(o.rank) {
		return
	}
	if !o.object {
		l.move(o.item, -o.n, -o.written)
	}
	if !r.object {
		l.move(r.item, o.n, o.written)
	}
	if l.needed(r) {
		o.rank = r
		l.own(string(key), o)
	} else {
		delete(l.owners, string(key))
	}
	l.held.Store(int64(len(l.owners)))
}

// needed reports whether a pass before r may still need a chunk r owns. It is
// called under l.mu.
func (l *ChunkCredits) needed(r rank) bool {
	return int64(r.item) > l.next.Load() || r.object && !l.ended[r.item]
}

func (l *ChunkCredits) own(key string, o chunkOwner) {
	l.owners[key] = o
	l.byItem[o.item] = append(l.byItem[o.item], key)
}

func (l *ChunkCredits) move(item int, n, written int64) {
	m := l.moved[item]
	m.new += n
	m.written += written
	l.moved[item] = m
}

// release lets go of the chunks item owns. It is called under l.mu.
func (l *ChunkCredits) release(item int) {
	for _, key := range l.byItem[item] {
		if o, ok := l.owners[key]; ok && o.item == item {
			delete(l.owners, key)
		}
	}
	delete(l.byItem, item)
}

// evidenceEnded records that the evidence pass of item ended. When no item
// before it runs, no pass before the objects its index still stores does
// either.
func (l *ChunkCredits) evidenceEnded(item int) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.ended[item] = true
	if int64(item) <= l.next.Load() {
		l.release(item)
	}
	l.held.Store(int64(len(l.owners)))
}

// Adjust is what item's evidence pass counted wrong as the run went: the
// bytes, and the bytes written, to add to its ChunkResult's NewBytes and
// WrittenBytes, negative for chunks a pass before it took. It is final once
// item and every item before it have ended.
func (l *ChunkCredits) Adjust(item int) (newBytes, written int64) {
	if l == nil {
		return 0, 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	m := l.moved[item]
	return m.new, m.written
}

// Settle says that item and every item before it have ended and been
// counted. The ledger lets go of what only their passes could take.
func (l *ChunkCredits) Settle(item int) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	for i := int(l.next.Load()); i <= item; i++ {
		l.release(i)
		delete(l.moved, i)
		delete(l.ended, i)
	}
	l.next.Store(max(l.next.Load(), int64(item)+1))
	l.held.Store(int64(len(l.owners)))
}

// Full reports whether the ledger holds as many chunks as it should. A run
// then starts no item but the first not yet ended, until Settle lets go of
// some.
func (l *ChunkCredits) Full() bool { return l != nil && l.held.Load() >= creditsHeld }

// CreditStats is what a ledger did so far.
type CreditStats struct {
	// Locked is how many times a pass storing or finding a chunk took the
	// ledger's lock: chunks stored ahead of an item not yet settled, and
	// chunks found while the ledger held any.
	Locked int64
	// Held is how many chunks the ledger holds now.
	Held int64
}

// Stats is what l did so far; the zero CreditStats for no ledger.
func (l *ChunkCredits) Stats() CreditStats {
	if l == nil {
		return CreditStats{}
	}
	return CreditStats{Locked: l.locked.Load(), Held: l.held.Load()}
}

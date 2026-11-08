package store

import (
	"sync"

	"indicer/lib/dbio"
	"indicer/lib/engine"
	"indicer/lib/fio"

	"github.com/dgraph-io/badger/v4"
)

// A chunk pass looks each chunk up in the database before it stores it, and
// the chunks it stores reach the database only as its pass ends. Passes of
// one database that run at once (the evidence of a folder stored side by
// side, an evidence and the objects its index stores) each missed a chunk
// they share and each stored it: in container mode a frame for every pass,
// of which one is pointed at and the rest are lost space nothing reclaims.
// So they share a chunkWriter. The first pass to store a chunk claims it;
// the others wait for it and record the metadata it stored the chunk under.
// In container mode they also share one container manager, so a pass's
// barrier, syncing it, makes durable a frame another pass wrote.
type chunkWriter struct {
	db   *badger.DB
	refs int // passes joined, under chunkWritersMu
	cm   *fio.ContainerManager

	mu     sync.Mutex
	claims map[string]*chunkClaim // by chunk key, until the claiming pass ends
}

// chunkClaim is a pass storing a chunk, then what it stored it under.
type chunkClaim struct {
	done chan struct{}
	meta []byte // nil when the database held the chunk already
	err  error
}

var (
	chunkWritersMu sync.Mutex
	chunkWriters   = map[*badger.DB]*chunkWriter{}
)

// passChunks is one pass's part in its database's chunkWriter.
type passChunks struct {
	w       *chunkWriter
	claimed []string // the claims this pass holds, under w.mu
	// credit counts the chunks the pass stores, or finds stored, in its run
	// (ChunkCredits).
	credit Credit
	// materialize encodes one chunk and writes its blob or container record:
	// dbio.MaterializeChonkNode, unless the pass's ChunkPass gave another.
	materialize materializeFunc
	// claimFound, when not nil, is called when the pass finds another's
	// claim, before it waits; tests set it to hold that claim's write until
	// then.
	claimFound func()
}

// materializeFunc writes one chunk, as dbio.MaterializeChonkNode does.
type materializeFunc = func(key, data []byte, db *badger.DB, c *fio.ContainerManager) ([]byte, int64, error)

// joinChunkWriter gives a pass its part in db's chunkWriter, which the first
// pass starts, with its container manager in container mode.
func joinChunkWriter(db *badger.DB) *passChunks {
	chunkWritersMu.Lock()
	defer chunkWritersMu.Unlock()
	w := chunkWriters[db]
	if w == nil {
		w = &chunkWriter{db: db, claims: map[string]*chunkClaim{}}
		if engine.For(db).Container() {
			w.cm = fio.NewContainerManager(dbio.KVDir(db))
		}
		chunkWriters[db] = w
	}
	w.refs++
	return &passChunks{w: w, materialize: dbio.MaterializeChonkNode}
}

// containers is the container manager the pass writes through, nil in file
// mode.
func (p *passChunks) containers() *fio.ContainerManager { return p.w.cm }

// leave ends the pass's part, once its batch is flushed or it failed: from
// then on the database answers for the chunks it claimed. The last pass to
// leave closes the container manager, and its error joins *err.
func (p *passChunks) leave(err *error) {
	w := p.w
	w.mu.Lock()
	for _, k := range p.claimed {
		delete(w.claims, k)
	}
	p.claimed = nil
	w.mu.Unlock()

	chunkWritersMu.Lock()
	w.refs--
	last := w.refs == 0
	if last {
		delete(chunkWriters, w.db)
	}
	chunkWritersMu.Unlock()
	if last && w.cm != nil {
		mergeDeferredErr(err, w.cm.Close())
	}
}

// store stores the chunk ckey with write, which returns the metadata to
// record for it, or nil when the database holds it already. While another
// pass of the database stores the chunk, or holds it stored and has not
// ended, store waits for that pass and returns the metadata it stored the
// chunk under, for this pass to record as well; fresh is then false, and
// written 0. A claim that failed is dropped: the next pass to need the chunk
// stores it itself.
func (p *passChunks) store(ckey []byte, write func() ([]byte, int64, error)) (meta []byte, written int64, fresh bool, err error) {
	w, k := p.w, string(ckey)
	for {
		w.mu.Lock()
		if c := w.claims[k]; c != nil {
			w.mu.Unlock()
			if p.claimFound != nil {
				p.claimFound()
			}
			<-c.done
			if c.err == nil {
				return c.meta, 0, false, nil
			}
			continue
		}
		c := &chunkClaim{done: make(chan struct{})}
		w.claims[k] = c
		w.mu.Unlock()

		c.meta, written, c.err = write()
		w.mu.Lock()
		if c.err != nil || c.meta == nil {
			delete(w.claims, k)
		} else {
			p.claimed = append(p.claimed, k)
		}
		w.mu.Unlock()
		close(c.done)
		return c.meta, written, c.err == nil && c.meta != nil, c.err
	}
}

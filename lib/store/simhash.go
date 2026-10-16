package store

import (
	"errors"
	"indicer/lib/cnst"
	"indicer/lib/dbio"
	"indicer/lib/util"
	"sync"

	"github.com/dgraph-io/badger/v4"
)

// chunkSimHash computes a chunk's signature; tests replace it.
var chunkSimHash = util.ChunkSimHash64

type simhashAsyncWriter struct {
	db      *badger.DB
	sem     chan struct{}
	wg      sync.WaitGroup
	errOnce sync.Once
	err     error
}

func newSimhashAsyncWriter(db *badger.DB, limit int) *simhashAsyncWriter {
	if limit < 1 {
		limit = 1
	}
	return &simhashAsyncWriter{
		db:  db,
		sem: make(chan struct{}, limit),
	}
}

// enqueue computes and stores the signature of one chunk in the background.
// It blocks while every worker slot is busy, so at most limit chunk copies
// exist at once whatever the size of the evidence (STO-13).
func (w *simhashAsyncWriter) enqueue(cdata, chash []byte) {
	w.sem <- struct{}{}
	// Copy at the async handoff so the goroutine does not hold caller-owned
	// slices (e.g. a mapped file sub-slice) past the caller's lifetime.
	cdataCopy := append([]byte(nil), cdata...)
	chash = append([]byte(nil), chash...)
	w.wg.Add(1)
	go func() {
		defer w.wg.Done()
		defer func() {
			<-w.sem
		}()

		sigKey := util.AppendToBytesSlice(cnst.ChonkSimhashNamespace, chash)
		err := dbio.PingNode(sigKey, w.db)
		if err == nil {
			return
		}
		if !errors.Is(err, badger.ErrKeyNotFound) {
			w.captureErr(err)
			return
		}

		sig := chunkSimHash(cdataCopy)
		if err = dbio.SetChonkSignature(chash, sig, w.db); err != nil {
			w.captureErr(err)
		}
	}()
}

func (w *simhashAsyncWriter) wait() error {
	w.wg.Wait()
	return w.err
}

func (w *simhashAsyncWriter) captureErr(err error) {
	if err == nil {
		return
	}
	w.errOnce.Do(func() {
		w.err = err
	})
}

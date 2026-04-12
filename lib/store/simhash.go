package store

import (
	"errors"
	"indicer/lib/cnst"
	"indicer/lib/dbio"
	"indicer/lib/util"
	"sync"

	"github.com/dgraph-io/badger/v4"
)

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

func (w *simhashAsyncWriter) enqueue(cdata, chash []byte) {
	w.wg.Add(1)
	go func() {
		defer w.wg.Done()

		w.sem <- struct{}{}
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

		sig := util.ChunkSimHash64(cdata)
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

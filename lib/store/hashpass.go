package store

import (
	"context"
	"io"
	"sync"

	"github.com/dgraph-io/badger/v4"
)

// ContentSums are the identity hashes of some content, as a chunk pass of it
// into db would compute them.
type ContentSums struct {
	Plain []byte
	Root  []byte
}

// HashContent reads size bytes of src and returns their plain hash and their
// content root in db's layout, storing nothing. Sync ingests run it before
// storing, to find content already stored; fingerprint candidates are
// confirmed with it (D1).
func HashContent(parent context.Context, db *badger.DB, src io.ReaderAt, size int64, parallel bool) (ContentSums, error) {
	shape := shapeFor(ChunkPass{DB: db, Parallel: parallel})
	ctx, cancel := context.WithCancel(parent)
	defer cancel()

	tap := newDigestTap(shape.fileHash, nil, nil)
	feed := shape.feed(ctx, ChunkPass{Source: src, Size: size}, tap)

	var mu sync.Mutex
	hocho := newHochoAccumulator(shape.fileHash, shape.chunk, 0)
	var hochoErr error
	var wg sync.WaitGroup
	for range shape.workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			h := shape.chunkHash()
			for c := range feed.chunks() {
				h.Reset()
				h.Write(c.data)
				chash := h.Sum(nil)
				off := c.off
				c.release()
				mu.Lock()
				if err := hocho.add(off, chash); err != nil && hochoErr == nil {
					hochoErr = err
					cancel()
				}
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if err := passErr(parent, feed.wait(), hochoErr); err != nil {
		return ContentSums{}, err
	}
	root, err := hocho.finalize()
	if err != nil {
		return ContentSums{}, err
	}
	return ContentSums{Plain: tap.plain.Sum(nil), Root: root}, nil
}

package fio

import (
	"errors"
	"sync"
)

const defaultAsyncFileWriteQueueDepth = 2048

type asyncFileWriteRequest struct {
	path string
	data []byte
}

type asyncFileWriter struct {
	queue []asyncFileWriteRequest
	head  int
	cond  *sync.Cond
	done  chan struct{}

	closeOnce sync.Once

	mu      sync.Mutex
	closed  bool
	running bool
	first   error
	backend writeBackend
}

var (
	asyncWriterMu sync.Mutex
	asyncWriter   *asyncFileWriter
)

func StartAsyncFileWriter(queueDepth int) {
	if queueDepth <= 0 {
		queueDepth = defaultAsyncFileWriteQueueDepth
	}

	asyncWriterMu.Lock()
	defer asyncWriterMu.Unlock()

	if asyncWriter != nil {
		return
	}

	w := &asyncFileWriter{
		queue:   make([]asyncFileWriteRequest, 0, queueDepth),
		head:    0,
		done:    make(chan struct{}),
		backend: getWriteBackend(),
	}
	w.cond = sync.NewCond(&w.mu)
	w.running = true
	asyncWriter = w
	go w.loop()
}

func EnqueueAsyncFileWrite(path string, data []byte) error {
	asyncWriterMu.Lock()
	w := asyncWriter
	asyncWriterMu.Unlock()

	if w == nil {
		return errors.New("async file writer is not started")
	}

	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed || !w.running {
		return errors.New("async file writer is closed")
	}
	w.queue = append(w.queue, asyncFileWriteRequest{path: path, data: data})
	w.cond.Signal()
	return nil
}

func CloseAsyncFileWriter() error {
	asyncWriterMu.Lock()
	w := asyncWriter
	asyncWriter = nil
	asyncWriterMu.Unlock()

	if w == nil {
		return nil
	}

	w.closeOnce.Do(func() {
		w.mu.Lock()
		w.closed = true
		w.cond.Broadcast()
		w.mu.Unlock()
	})

	<-w.done

	w.mu.Lock()
	defer w.mu.Unlock()
	return w.first
}

func (w *asyncFileWriter) loop() {
	defer close(w.done)
	for {
		w.mu.Lock()
		for w.head >= len(w.queue) && !w.closed {
			w.cond.Wait()
		}

		if w.head >= len(w.queue) && w.closed {
			w.running = false
			w.mu.Unlock()
			return
		}

		req := w.queue[w.head]
		w.head++
		if w.head > 0 && (w.head >= len(w.queue)/2 || w.head >= 1024) {
			remaining := len(w.queue) - w.head
			if remaining > 0 {
				copy(w.queue[:remaining], w.queue[w.head:])
			}
			w.queue = w.queue[:remaining]
			w.head = 0
		}
		w.mu.Unlock()

		if err := w.backend.WriteBlob(req.path, req.data); err != nil {
			w.mu.Lock()
			if w.first == nil {
				w.first = err
			}
			w.mu.Unlock()
		}
	}
}

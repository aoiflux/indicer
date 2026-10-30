package store

import (
	"bufio"
	"bytes"
	"context"
	"crypto/md5"
	"crypto/sha1"
	"crypto/sha3"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"hash"
	"indicer/internal/console"
	"indicer/internal/faultinject"
	"indicer/lib/cnst"
	"indicer/lib/dbio"
	"indicer/lib/fio"
	"indicer/lib/structs"
	"io"
	"sync"
	"time"

	"github.com/dgraph-io/badger/v4"
	"github.com/dustin/go-humanize"
	"github.com/schollz/progressbar/v3"
	"github.com/zeebo/blake3"
)

// VerifyLevel is how much a restore checks what it writes.
type VerifyLevel int

const (
	// VerifyFull checks every chunk against its hash, and the whole object
	// against the hash its record carries.
	VerifyFull VerifyLevel = iota
	// VerifyChunks checks every chunk against its hash.
	VerifyChunks
	// VerifyOff checks nothing beyond what decoding a chunk checks: the v2
	// encoding always checks a chunk against its hash, legacy encodings do
	// not.
	VerifyOff
)

var verifyLevelNames = map[VerifyLevel]string{VerifyFull: "full", VerifyChunks: "chunks", VerifyOff: "off"}

func (v VerifyLevel) String() string { return verifyLevelNames[v] }

// ParseVerifyLevel parses "full", "chunks" or "off".
func ParseVerifyLevel(s string) (VerifyLevel, error) {
	for v, name := range verifyLevelNames {
		if s == name {
			return v, nil
		}
	}
	return 0, fmt.Errorf("unknown verification level %q: must be full, chunks or off", s)
}

// RestoreOptions control a restore.
type RestoreOptions struct {
	Verify VerifyLevel
	// VerifyEWF checks a restored EWF evidence against the MD5 and SHA-1 the
	// set recorded at acquisition, when it recorded any.
	VerifyEWF bool
}

// DigestStatus says what a restore found out about the whole object.
type DigestStatus string

const (
	// DigestVerified: the restored content has the hash its record carries.
	DigestVerified DigestStatus = "verified"
	// DigestUnavailable: the record carries no hash restore can compute.
	DigestUnavailable DigestStatus = "unavailable"
	// DigestSkipped: the restore was not asked to check the whole object.
	DigestSkipped DigestStatus = "skipped"
)

// ErrDigestMismatch reports restored content that does not have the hash its
// record carries, although every chunk matched its own hash: the object's
// relations or its recorded range are wrong.
var ErrDigestMismatch = errors.New("restored content does not match the hash its record carries")

// RestoreReport describes a finished restore.
type RestoreReport struct {
	Key    []byte // the restored object's record key
	Size   int64
	Chunks int64
	Digest DigestStatus
	// DigestForm names the hash that matched, such as "blake3" or
	// "hocho-sha3-256".
	DigestForm string
	// RecordedSize is the size the record gives. A file whose content could
	// only partly be located restores the Size bytes that were (Truncated).
	RecordedSize int64
	Truncated    bool
	// EWF is the result of checking an EWF evidence against the digests its
	// set recorded (structs.EWFVerified or EWFMismatch), or "" when it was
	// not checked.
	EWF string
}

type restoreChunkJob struct {
	seq  int64
	span dbio.ChunkSpan
}

type restoreChunkResult struct {
	seq  int64
	span dbio.ChunkSpan
	data []byte
	err  error
}

type restoreProducerResult struct {
	total int64
	err   error
}

// errRestoreStopped ends the producer when the restore stops early.
var errRestoreStopped = errors.New("restore stopped")

// Restore writes the content of the object whose hash is rhash to dst,
// checking it as opts asks. The object's evidence must have finished
// ingesting. A cancelled ctx stops it with ctx's error; dst then holds a
// partial write, as it does after any error.
func Restore(ctx context.Context, db *badger.DB, rhash string, dst io.Writer, opts RestoreOptions) (RestoreReport, error) {
	if err := ctx.Err(); err != nil {
		return RestoreReport{}, err
	}
	fid, err := dbio.GuessFileType(rhash, db)
	if err != nil {
		return RestoreReport{}, err
	}
	l, err := dbio.OpenLogical(db, fid, dbio.ReadOptions{SkipVerify: opts.Verify == VerifyOff})
	if err != nil {
		return RestoreReport{}, err
	}
	if !l.Completed() {
		return RestoreReport{}, cnst.ErrIncompleteFile
	}
	var ewf *structs.EWFCheck
	if opts.VerifyEWF && bytes.HasPrefix(fid, []byte(cnst.EviFileNamespace)) {
		rec, err := dbio.GetEvidenceFile(fid, db)
		if err != nil {
			return RestoreReport{}, err
		}
		if rec.Source != nil && rec.Source.EWF != nil && (len(rec.Source.EWF.StoredMD5) > 0 || len(rec.Source.EWF.StoredSHA1) > 0) {
			ewf = rec.Source.EWF
		}
	}
	return restoreLogical(ctx, l, dst, opts, ewf)
}

// restoreLogical writes the content of l to dst. Chunks are read and checked
// in parallel and written in order; at most the window is between being read
// and being written (RST-5).
func restoreLogical(ctx context.Context, l *dbio.Logical, dst io.Writer, opts RestoreOptions, ewf *structs.EWFCheck) (RestoreReport, error) {
	report := RestoreReport{Key: l.Key(), Size: l.Size(), RecordedSize: l.RecordedSize(), Truncated: l.Truncated(), Digest: DigestSkipped}
	configureRestoreCache()

	fio.EnableContainerReadCache()
	defer fio.DisableContainerReadCache()

	writer := bufio.NewWriterSize(dst, cnst.GetRestoreWriteBufferSize())
	var digest *digester
	if opts.Verify == VerifyFull {
		digest = newDigester(l)
	}
	var ewfMD5, ewfSHA1 hash.Hash
	if ewf != nil {
		ewfMD5, ewfSHA1 = md5.New(), sha1.New()
	}

	bar := progressbar.NewOptions64(
		l.Size(),
		progressbar.OptionShowBytes(true),
		progressbar.OptionSetWriter(console.Progress()),
		progressbar.OptionSetTheme(cnst.CommonProgressBarTheme),
	)

	workerCount, jobQueueDepth := cnst.GetRestorePipelineTuning()
	fetchBatchSize, window := restoreWindow(l.ChunkSize())
	slots := make(chan struct{}, window)
	jobs := make(chan restoreChunkJob, jobQueueDepth)
	results := make(chan restoreChunkResult, jobQueueDepth)
	producerDone := make(chan restoreProducerResult, 1)

	done := make(chan struct{})
	var doneOnce sync.Once
	stop := func() {
		doneOnce.Do(func() {
			close(done)
		})
	}
	// Cancelling ctx stops the producer and the workers like an error does.
	stopOnCancel := context.AfterFunc(ctx, stop)
	defer stopOnCancel()

	var workers sync.WaitGroup
	workers.Add(workerCount)
	for i := 0; i < workerCount; i++ {
		go func() {
			defer workers.Done()
			for {
				select {
				case <-done:
					return
				case job, ok := <-jobs:
					if !ok {
						return
					}

					data, err := readRestoreSpan(l, job.span)
					result := restoreChunkResult{seq: job.seq, span: job.span, data: data, err: err}

					select {
					case results <- result:
					case <-done:
						return
					}

					if err != nil {
						return
					}
				}
			}
		}()
	}

	go func() {
		defer close(jobs)
		var totalSeq int64
		err := l.Spans(ctx, fetchBatchSize, func(spans []dbio.ChunkSpan) error {
			batchSeqStart := totalSeq
			totalSeq += int64(len(spans))
			for _, i := range containerLocalityOrder(spans) {
				job := restoreChunkJob{seq: batchSeqStart + int64(i), span: spans[i]}
				// Take a window slot first: the writer frees it once this
				// chunk is written, so reads never run more than the window
				// ahead of the write position.
				select {
				case slots <- struct{}{}:
				case <-done:
					return errRestoreStopped
				}
				select {
				case jobs <- job:
				case <-done:
					return errRestoreStopped
				}
			}
			return nil
		})
		if errors.Is(err, errRestoreStopped) {
			err = nil
		}
		producerDone <- restoreProducerResult{total: totalSeq, err: err}
	}()

	go func() {
		workers.Wait()
		close(results)
	}()

	progressInterval := cnst.GetRestoreProgressInterval()
	lastProgress := time.Now()
	var pendingProgress, restored int64
	nextSeq := int64(0)
	pending := make(map[int64]restoreChunkResult)
	var workerErr error

	write := func(r restoreChunkResult) error {
		if _, err := writer.Write(r.data); err != nil {
			return err
		}
		if digest != nil {
			digest.add(r.span, r.data)
		}
		if ewfMD5 != nil {
			ewfMD5.Write(r.data)
			ewfSHA1.Write(r.data)
		}
		<-slots
		pendingProgress += int64(len(r.data))
		restored += int64(len(r.data))
		nextSeq++
		return faultinject.Hit(faultinject.RestoreMid)
	}

	for result := range results {
		if result.err != nil {
			if workerErr == nil {
				workerErr = result.err
				stop()
			}
			continue
		}
		if workerErr != nil {
			continue
		}

		if result.seq == nextSeq {
			err := write(result)
			for err == nil {
				next, ok := pending[nextSeq]
				if !ok {
					break
				}
				delete(pending, nextSeq)
				err = write(next)
			}
			if err != nil {
				workerErr = err
				stop()
				continue
			}
		} else if result.seq > nextSeq {
			pending[result.seq] = result
			notePendingChunks(len(pending))
		}

		if pendingProgress >= 4*cnst.MB || time.Since(lastProgress) >= progressInterval {
			bar.Add64(pendingProgress)
			pendingProgress = 0
			lastProgress = time.Now()
		}
	}

	producerResult := <-producerDone
	if err := ctx.Err(); err != nil {
		return report, err
	}
	if workerErr != nil {
		return report, workerErr
	}
	if producerResult.err != nil {
		return report, producerResult.err
	}
	if nextSeq != producerResult.total {
		return report, fmt.Errorf("restore pipeline incomplete: wrote %d/%d chunks", nextSeq, producerResult.total)
	}
	if restored != l.Size() {
		return report, fmt.Errorf("restore wrote %d bytes, the record's size is %d", restored, l.Size())
	}
	report.Chunks = nextSeq

	if pendingProgress > 0 {
		bar.Add64(pendingProgress)
	}
	if err := writer.Flush(); err != nil {
		return report, err
	}
	if digest != nil {
		report.Digest, report.DigestForm = digest.result()
		if report.Digest == "" {
			return report, fmt.Errorf("%w: %x records %s", ErrDigestMismatch, l.Key(), l.FileHash())
		}
	}
	if ewf != nil {
		// A set whose digests did not match at ingest restores the bytes it
		// holds; the report says they differ from what the imager hashed.
		report.EWF = structs.EWFVerified
		if (len(ewf.StoredMD5) > 0 && !bytes.Equal(ewfMD5.Sum(nil), ewf.StoredMD5)) ||
			(len(ewf.StoredSHA1) > 0 && !bytes.Equal(ewfSHA1.Sum(nil), ewf.StoredSHA1)) {
			report.EWF = structs.EWFMismatch
		}
	}

	bar.Finish()
	fmt.Fprintln(console.Err())
	console.Println("Restored file with size: ", humanize.Bytes(uint64(l.Size())))
	// A progress bar error (an empty input has nothing to show) is not the
	// operation's error.
	_ = bar.Close()
	return report, nil
}

// Test seams.
var (
	readRestoreSpan   = (*dbio.Logical).ReadSpan
	notePendingChunks = func(int) {}
)

// restoreWindowBytes sizes a restore's lookup batch (RST-5). The window of
// chunks being read, queued, or parked until their turn is twice the batch,
// so it holds up to 2 x restoreWindowBytes: 256 MiB up to 1 MiB chunks, and
// 512 MiB from 2 MiB chunks on.
const restoreWindowBytes = 256 * cnst.MB

// restoreWindow returns how many chunks of chunkSize the producer looks up
// per read transaction, and how many may be between lookup and write at
// once. The window is twice the batch: the producer emits a batch in storage
// order, so the next chunk to write may be the last one emitted, and a
// window smaller than a batch could stall with that chunk never sent.
func restoreWindow(chunkSize int64) (batch, window int) {
	batch = int(min(128, max(4, int64(restoreWindowBytes)/chunkSize)))
	return batch, 2 * batch
}

func configureRestoreCache() {
	cacheSize, err := cnst.GetCacheLimit()
	if err != nil {
		cacheSize = 256 * cnst.MB
	}

	maxCache := int64(4 * cnst.GB)
	if cacheSize > maxCache {
		cacheSize = maxCache
	}
	fio.SetContainerReadCacheSize(cacheSize)
}

// containerLocalityOrder returns the order to read a batch's spans in:
// chunks of one container together, for cache and read locality. The
// sequence numbers, and so the write order, are unchanged.
func containerLocalityOrder(spans []dbio.ChunkSpan) []int {
	containerGroups := make(map[string][]int)
	groupOrder := make([]string, 0, 8)
	order := make([]int, 0, len(spans))
	var nonContainer []int

	for i, s := range spans {
		if !s.Loc.Container || s.Loc.Path == "" {
			nonContainer = append(nonContainer, i)
			continue
		}
		if _, exists := containerGroups[s.Loc.Path]; !exists {
			groupOrder = append(groupOrder, s.Loc.Path)
		}
		containerGroups[s.Loc.Path] = append(containerGroups[s.Loc.Path], i)
	}
	if len(groupOrder) <= 1 {
		for i := range spans {
			order = append(order, i)
		}
		return order
	}
	for _, path := range groupOrder {
		order = append(order, containerGroups[path]...)
	}
	return append(order, nonContainer...)
}

// digester computes, while a restore writes, the hash forms an object's
// recorded hash can take, so the whole object can be checked against it:
//
//   - plain: the file hash of the content (sync and async strategies, and
//     partitions and indexed files hashed directly);
//   - hocho: the file hash over each chunk's hash, length first, the way the
//     hocho strategy hashes evidence;
//   - hocho with edges: the same, but a chunk the object covers only partly,
//     or a short last chunk, contributes the file hash of the covered piece,
//     the way partitions and indexed files are hashed from relations.
//
// The file hash is BLAKE3 in a BLAKE3 database and SHA3-256 in a SHA3 one;
// the chunk hash's length says which (64 bytes is SHA3-512).
type digester struct {
	want   []byte
	stride int64
	chunks *dbio.EvidenceChunks
	algos  []fileHashAlgo
}

type fileHashAlgo struct {
	name                string
	plain, hocho, edges hash.Hash
	newHash             func() hash.Hash
}

func newFileHashAlgo(name string, newHash func() hash.Hash) fileHashAlgo {
	return fileHashAlgo{name: name, plain: newHash(), hocho: newHash(), edges: newHash(), newHash: newHash}
}

func newBLAKE3() hash.Hash  { return blake3.New() }
func newSHA3256() hash.Hash { return sha3.New256() }

func newDigester(l *dbio.Logical) *digester {
	want, err := base64.StdEncoding.DecodeString(l.FileHash())
	if err != nil || len(want) != 32 {
		// No hash, or none in a form restore computes.
		want = nil
	}
	return &digester{want: want, stride: l.ChunkSize(), chunks: l.Evidence()}
}

func (d *digester) add(s dbio.ChunkSpan, data []byte) {
	if d.want == nil {
		return
	}
	if !s.Stored() {
		// Holes and inline bytes: only an indexed file recorded since W6 has
		// them, and its hash is a plain one.
		if d.algos == nil {
			d.algos = []fileHashAlgo{newFileHashAlgo("blake3", newBLAKE3), newFileHashAlgo("sha3-256", newSHA3256)}
		}
		for _, a := range d.algos {
			a.plain.Write(data)
		}
		return
	}
	if d.algos == nil {
		switch len(s.Hash) {
		case 64:
			d.algos = []fileHashAlgo{newFileHashAlgo("sha3-256", newSHA3256)}
		case 32:
			d.algos = []fileHashAlgo{newFileHashAlgo("blake3", newBLAKE3)}
		default:
			d.algos = []fileHashAlgo{newFileHashAlgo("blake3", newBLAKE3), newFileHashAlgo("sha3-256", newSHA3256)}
		}
	}
	whole := s.Lo == 0 && s.Hi == d.chunks.ChunkLen(s.Chunk)
	full := s.Lo == 0 && s.Hi == d.stride
	for _, a := range d.algos {
		a.plain.Write(data)
		var piece []byte
		if !whole || !full {
			h := a.newHash()
			h.Write(data)
			piece = h.Sum(nil)
		}
		writeHochoEntry(a.hocho, s.Hash, piece, whole)
		writeHochoEntry(a.edges, s.Hash, piece, full)
	}
}

func writeHochoEntry(h hash.Hash, chunkHash, piece []byte, useChunkHash bool) {
	entry := piece
	if useChunkHash {
		entry = chunkHash
	}
	var n [4]byte
	binary.BigEndian.PutUint32(n[:], uint32(len(entry)))
	h.Write(n[:])
	h.Write(entry)
}

// result returns DigestVerified and the form that matched, DigestUnavailable
// when there is no hash to check, or "" when the content does not match it.
func (d *digester) result() (DigestStatus, string) {
	if d.want == nil {
		return DigestUnavailable, ""
	}
	if d.algos == nil {
		// An empty object: only the plain hash of nothing is defined.
		d.algos = []fileHashAlgo{newFileHashAlgo("blake3", newBLAKE3), newFileHashAlgo("sha3-256", newSHA3256)}
	}
	for _, a := range d.algos {
		switch {
		case bytes.Equal(a.plain.Sum(nil), d.want):
			return DigestVerified, a.name
		case bytes.Equal(a.hocho.Sum(nil), d.want):
			return DigestVerified, "hocho-" + a.name
		case bytes.Equal(a.edges.Sum(nil), d.want):
			return DigestVerified, "hocho-edges-" + a.name
		}
	}
	return "", ""
}

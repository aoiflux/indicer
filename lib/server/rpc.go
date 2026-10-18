package server

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"net/http"
	"runtime/debug"
	"sort"
	"time"

	"indicer/lib/cnst"
	"indicer/lib/dbio"
	"indicer/lib/structs"
	"indicer/pb"
	"indicer/pb/pbconnect"

	"connectrpc.com/connect"
	"github.com/dgraph-io/badger/v4"
	"go.uber.org/zap"
)

const (
	// unaryReadMax bounds a unary request message, after decompression, so a
	// compressed request cannot expand without limit.
	unaryReadMax = 1 << 20
	// streamReadMax bounds one StreamFile message: a chunk and its framing.
	streamReadMax = maxChunk + 64<<10
	// sendMax bounds one response message.
	sendMax = 64 << 20

	defaultPageSize = 1000
	maxPageSize     = 10000
	// maxChunkEntries bounds the chunk-map entries of one response. An entry
	// encodes to about 52 bytes, so 1M of them fit under sendMax.
	maxChunkEntries = 1 << 20
	// maxSearchHits bounds the objects one search response counts.
	maxSearchHits = 100_000
	// searchBudget bounds the time one search request may take (SRC-2).
	searchBudget = 2 * time.Minute
	// searchSlots is how many searches run at once.
	searchSlots = 2
	// maxClientPath bounds a path a client gives for a file.
	maxClientPath = 4096
)

// Seams for tests.
var (
	chunkEntriesPerResponse = maxChunkEntries
	openLogical             = dbio.OpenLogical
)

// rpcService is the DUES service on the server's database.
type rpcService struct {
	s        *Server
	searches chan struct{}
}

var _ pbconnect.DuesServiceHandler = (*rpcService)(nil)

func (r *rpcService) db() *badger.DB { return r.s.deps.DB }

// rpcRoutes registers each procedure with the bounds of what it does.
func (s *Server) rpcRoutes() {
	svc := &rpcService{s: s, searches: make(chan struct{}, searchSlots)}
	common := []connect.HandlerOption{
		connect.WithRecover(s.recoverRPC),
		connect.WithInterceptors(rpcErrors{s}),
		connect.WithSendMaxBytes(sendMax),
	}
	_, unary := pbconnect.NewDuesServiceHandler(svc, append(common, connect.WithReadMaxBytes(unaryReadMax))...)
	_, stream := pbconnect.NewDuesServiceHandler(svc, append(common, connect.WithReadMaxBytes(streamReadMax))...)

	listing := route{maxBody: 2 * unaryReadMax, read: 30 * time.Second, write: 5 * time.Minute}
	for _, p := range []string{
		pbconnect.DuesServiceAppendIfExistsProcedure,
		pbconnect.DuesServiceGetEviFilesProcedure,
		pbconnect.DuesServiceGetPartiFilesProcedure,
		pbconnect.DuesServiceGetIdxFilesProcedure,
	} {
		s.handle(s.mux, "POST "+p, listing, unary)
	}
	s.handle(s.mux, "POST "+pbconnect.DuesServiceSearchProcedure,
		route{maxBody: 2 * unaryReadMax, read: 30 * time.Second, write: searchBudget + 30*time.Second}, unary)
	// An upload's body may take long, but must keep moving; its ingest may
	// take long too, so the response has no deadline of its own.
	s.handle(s.mux, "POST "+pbconnect.DuesServiceStreamFileProcedure, route{idleRead: time.Minute}, stream)
}

// recoverRPC turns a handler panic into an internal error naming only the
// request ID; the panic and its stack go to the log.
func (s *Server) recoverRPC(ctx context.Context, spec connect.Spec, _ http.Header, p any) error {
	id := RequestID(ctx)
	stack := debug.Stack()
	if len(stack) > 64<<10 {
		stack = stack[:64<<10]
	}
	s.log.Error("server PANIC", zap.String("request_id", id), zap.String("procedure", spec.Procedure),
		zap.String("panic", fmt.Sprint(p)), zap.ByteString("stack", stack))
	return connect.NewError(connect.CodeInternal, fmt.Errorf("internal error (request %s)", id))
}

// rpcErrors makes every error a handler returns a Connect error. Errors the
// handlers already coded pass; cancellation and deadlines get their codes;
// anything else is logged and answered as an internal error naming only the
// request ID, so internal detail does not reach clients (SRV-6).
type rpcErrors struct{ s *Server }

func (e rpcErrors) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		res, err := next(ctx, req)
		return res, e.s.rpcError(ctx, req.Spec().Procedure, err)
	}
}

func (e rpcErrors) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}

func (e rpcErrors) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return func(ctx context.Context, conn connect.StreamingHandlerConn) error {
		return e.s.rpcError(ctx, conn.Spec().Procedure, next(ctx, conn))
	}
}

func (s *Server) rpcError(ctx context.Context, op string, err error) error {
	if err == nil {
		return nil
	}
	var ce *connect.Error
	switch {
	case errors.As(err, &ce):
		return err
	case errors.Is(err, context.DeadlineExceeded):
		return connect.NewError(connect.CodeDeadlineExceeded, errors.New("the request ran out of time"))
	case errors.Is(err, context.Canceled):
		return connect.NewError(connect.CodeCanceled, errors.New("the request was cancelled"))
	}
	id := RequestID(ctx)
	s.log.Error("server rpc failed", zap.String("request_id", id), zap.String("procedure", op), zap.Error(err))
	return connect.NewError(connect.CodeInternal, fmt.Errorf("internal error (request %s)", id))
}

func invalidArg(format string, args ...any) error {
	return connect.NewError(connect.CodeInvalidArgument, fmt.Errorf(format, args...))
}

func notFound(format string, args ...any) error {
	return connect.NewError(connect.CodeNotFound, fmt.Errorf(format, args...))
}

// page is one page request: at most n objects after the one the token names.
type page struct {
	n     int
	after []byte
}

func readPage(size int32, token string) (page, error) {
	p := page{n: int(size)}
	switch {
	case size < 0:
		return p, invalidArg("page_size must not be negative")
	case size == 0:
		p.n = defaultPageSize
	case size > maxPageSize:
		p.n = maxPageSize
	}
	if token != "" {
		b, err := base64.RawURLEncoding.DecodeString(token)
		if err != nil || len(b) == 0 {
			return p, invalidArg("page_token is not one this server gave")
		}
		p.after = b
	}
	return p, nil
}

func pageToken(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// chunkBudget counts the chunk-map entries of one response.
type chunkBudget struct{ left int }

// baseFile describes the object key for a listing: its chunk map holds at
// most the budget's remaining entries. ok is false when the object would not
// fit in what is left of a page that already holds others: it then starts
// the next page.
func (r *rpcService) baseFile(ctx context.Context, key []byte, path string, size int64, b *chunkBudget, first bool) (*pb.BaseFile, bool, error) {
	limit := b.left
	if first {
		limit = max(limit, 1)
	}
	m, truncated, err := chunkMap(ctx, r.db(), key, limit)
	if err != nil {
		return nil, false, fmt.Errorf("chunk map of %q: %w", path, err)
	}
	if truncated && !first {
		return nil, false, nil
	}
	b.left -= len(m)
	return &pb.BaseFile{
		FilePath:          path,
		FileId:            base64.StdEncoding.EncodeToString(key),
		FileSize:          size,
		ChunkMap:          m,
		ChunkMapTruncated: truncated,
	}, true, nil
}

// errEnough stops a span walk once a chunk map is full.
var errEnough = errors.New("enough chunks")

// chunkMap maps the base64 hash of each stored chunk holding part of the
// object key (an evidence, partition or indexed file record) to its stored
// size, as its location records it; a chunk only partly in the object counts
// whole, as it is stored whole. Holes and inline content have no chunk. It
// holds at most limit chunks, and truncated says whether there were more. An
// object whose content cannot be read has no chunks.
func chunkMap(ctx context.Context, db *badger.DB, key []byte, limit int) (m map[string]int64, truncated bool, err error) {
	m = map[string]int64{}
	l, err := openLogical(db, key, dbio.ReadOptions{})
	if errors.Is(err, dbio.ErrContentUnavailable) {
		return m, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	err = l.Spans(ctx, dbio.SpanBatch(l.ChunkSize()), func(spans []dbio.ChunkSpan) error {
		for _, s := range spans {
			if !s.Stored() {
				continue
			}
			h := base64.StdEncoding.EncodeToString(s.Hash)
			if _, ok := m[h]; ok {
				continue
			}
			if len(m) >= limit {
				truncated = true
				return errEnough
			}
			m[h] = s.Loc.StoredSize
		}
		return nil
	})
	if errors.Is(err, errEnough) {
		err = nil
	}
	return m, truncated, err
}

func completed(e structs.EvidenceFile) bool {
	return (e.Completed || e.IngestState == structs.IngestStateCompleted) && e.IngestState != structs.IngestStateDuplicate
}

func quarantined(e structs.EvidenceFile) bool {
	return e.Index != nil && e.Index.State == structs.IndexInvalid
}

// GetEviFiles lists the stored evidence in key order (the order it was
// stored in), a page at a time.
func (r *rpcService) GetEviFiles(ctx context.Context, req *connect.Request[pb.GetEviFilesReq]) (*connect.Response[pb.GetEviFilesRes], error) {
	pg, err := readPage(req.Msg.GetPageSize(), req.Msg.GetPageToken())
	if err != nil {
		return nil, err
	}
	type ev struct {
		key []byte
		rec structs.EvidenceFile
	}
	// One more than the page, to know whether another page follows.
	var evs []ev
	err = r.db().View(func(txn *badger.Txn) error {
		prefix := []byte(cnst.EviFileNamespace)
		opts := badger.DefaultIteratorOptions
		opts.Prefix = prefix
		it := txn.NewIterator(opts)
		defer it.Close()
		it.Seek(prefix)
		if pg.after != nil {
			it.Seek(pg.after)
		}
		for ; it.Valid() && len(evs) <= pg.n; it.Next() {
			if err := ctx.Err(); err != nil {
				return err
			}
			key := it.Item().KeyCopy(nil)
			if pg.after != nil && bytes.Compare(key, pg.after) <= 0 {
				continue
			}
			val, err := it.Item().ValueCopy(nil)
			if err != nil {
				return err
			}
			var e structs.EvidenceFile
			if err := dbio.DecodeRecord(val, &e); err != nil {
				return fmt.Errorf("evidence record %x: %w", key, err)
			}
			if completed(e) {
				evs = append(evs, ev{key, e})
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	res := &pb.GetEviFilesRes{Done: true}
	b := &chunkBudget{left: chunkEntriesPerResponse}
	for i, e := range evs {
		if i == pg.n {
			res.NextPageToken = pageToken(evs[i-1].key)
			break
		}
		bf, ok, err := r.baseFile(ctx, e.key, e.rec.Name, e.rec.Size, b, i == 0)
		if err != nil {
			return nil, err
		}
		if !ok {
			res.NextPageToken = pageToken(evs[i-1].key)
			break
		}
		res.EviFile = append(res.EviFile, bf)
	}
	return connect.NewResponse(res), nil
}

// getEvidence reads the evidence a client names by its record key or raw
// ID (base64); only stored evidence is found.
func (r *rpcService) getEvidence(id string) ([]byte, structs.EvidenceFile, error) {
	raw, err := base64.StdEncoding.DecodeString(id)
	if err != nil || len(raw) == 0 {
		return nil, structs.EvidenceFile{}, invalidArg("evi_file_id is not a base64 record ID")
	}
	key := dbio.CanonicalEvidenceKey(raw)
	e, err := dbio.GetEvidenceFile(key, r.db())
	if errors.Is(err, badger.ErrKeyNotFound) || (err == nil && !completed(e)) {
		return nil, e, notFound("no stored evidence %s", id)
	}
	return key, e, err
}

// GetPartiFiles lists an evidence's partitions in the order they lie in it.
// An index quarantined as wrong (repair --upgrade) lists none.
func (r *rpcService) GetPartiFiles(ctx context.Context, req *connect.Request[pb.GetPartiFilesReq]) (*connect.Response[pb.GetPartiFilesRes], error) {
	if req.Msg.GetEviFileId() == "" {
		return nil, invalidArg("evi_file_id is required")
	}
	pg, err := readPage(req.Msg.GetPageSize(), req.Msg.GetPageToken())
	if err != nil {
		return nil, err
	}
	_, e, err := r.getEvidence(req.Msg.GetEviFileId())
	if err != nil {
		return nil, err
	}
	res := &pb.GetPartiFilesRes{Done: true}
	if quarantined(e) {
		return connect.NewResponse(res), nil
	}
	type part struct {
		key []byte
		rec structs.PartitionFile
		pos []byte // the page position: start, then key
	}
	var parts []part
	for id := range e.InternalObjects {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		raw, err := base64.StdEncoding.DecodeString(id)
		if err != nil {
			return nil, fmt.Errorf("partition link %q: %w", id, err)
		}
		key := dbio.CanonicalPartitionKey(raw)
		p, err := dbio.GetPartitionFile(key, r.db())
		if errors.Is(err, badger.ErrKeyNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		pos := append(binary.BigEndian.AppendUint64(nil, uint64(max(p.Start, 0))), key...)
		parts = append(parts, part{key: key, rec: p, pos: pos})
	}
	sort.Slice(parts, func(i, j int) bool { return bytes.Compare(parts[i].pos, parts[j].pos) < 0 })
	b := &chunkBudget{left: chunkEntriesPerResponse}
	var last []byte
	for _, p := range parts {
		if pg.after != nil && bytes.Compare(p.pos, pg.after) <= 0 {
			continue
		}
		if len(res.PartitionFile) == pg.n {
			res.NextPageToken = pageToken(last)
			break
		}
		bf, ok, err := r.baseFile(ctx, p.key, dbio.RecordPath(p.rec.IndexedFile), p.rec.Size, b, len(res.PartitionFile) == 0)
		if err != nil {
			return nil, err
		}
		if !ok {
			res.NextPageToken = pageToken(last)
			break
		}
		res.PartitionFile = append(res.PartitionFile, bf)
		last = p.pos
	}
	return connect.NewResponse(res), nil
}

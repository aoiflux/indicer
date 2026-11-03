package server

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"hash"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"indicer/internal/diskspace"
	"indicer/internal/faultinject"
	"indicer/lib/cnst"
	"indicer/lib/dbio"
	"indicer/lib/dbmeta"
	"indicer/lib/ingest"
	"indicer/lib/store"
	"indicer/lib/structs"
	"indicer/pb"

	"connectrpc.com/connect"
	"github.com/dgraph-io/badger/v4"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// Uploads (SRV-2, SRV-3, SRV-4). A session receives a file into
// <root>/uploads/<id>.part, at the offsets the client names, and is
// finalized once: its bytes are checked against the client's hash and
// ingested, and the result is kept for later calls. docs/SERVER_API.md
// describes the protocol.

// maxChunk bounds one chunk of an upload.
const maxChunk = 16 << 20

// UploadLimits bound the uploads the server takes on.
type UploadLimits struct {
	MaxSessions int   // sessions receiving or finalizing at once
	MaxSize     int64 // declared size of one upload
	// IdleTTL ends a session that receives nothing for this long;
	// MaxLifetime ends one this long after it started, active or not.
	IdleTTL, MaxLifetime time.Duration
	// DoneTTL is how long a finished session's result is kept.
	DoneTTL time.Duration
	// FinalizeWait is how long a finalize call waits for the ingest before
	// answering 202; the result is then had from status or finalize.
	FinalizeWait time.Duration
	ReapInterval time.Duration
	// Headroom is the free space wanted beyond an upload's declared size.
	Headroom int64
}

// DefaultUploadLimits are the limits `dues server` runs with.
func DefaultUploadLimits() UploadLimits {
	return UploadLimits{
		MaxSessions:  16,
		MaxSize:      256 << 30,
		IdleTTL:      30 * time.Minute,
		MaxLifetime:  24 * time.Hour,
		DoneTTL:      10 * time.Minute,
		FinalizeWait: 25 * time.Second,
		ReapInterval: time.Minute,
		Headroom:     diskspace.Reserve,
	}
}

// Session states.
const (
	stateReceiving  = "receiving"
	stateFinalizing = "finalizing"
	stateDone       = "done"
	stateFailed     = "failed"
)

// uploadError is a refusal with its HTTP status; offset, when not negative,
// is the offset the session holds, which the client should send from.
type uploadError struct {
	status int
	msg    string
	offset int64
}

func (e *uploadError) Error() string { return e.msg }

func uploadErr(status int, format string, args ...any) *uploadError {
	return &uploadError{status: status, msg: fmt.Sprintf(format, args...), offset: -1}
}

type session struct {
	id         string
	path       string // the part file
	f          *os.File
	clientPath string
	fileType   string
	hash       string // the client's, base64
	sum        hash.Hash
	size       int64 // declared; -1 when not (a StreamFile without size)

	// Guarded by uploads.mu.
	offset   int64
	created  time.Time
	last     time.Time
	finished time.Time
	state    string
	result   *uploadResult
	err      *uploadError

	op   chan struct{} // held by the one operation running on the session
	done chan struct{} // closed when its finalize ends
}

func (s *session) lock(ctx context.Context) error {
	select {
	case s.op <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *session) tryLock() bool {
	select {
	case s.op <- struct{}{}:
		return true
	default:
		return false
	}
}

func (s *session) unlock() { <-s.op }

// uploadResult is what a finalized upload gives back.
type uploadResult struct {
	Done     bool   `json:"done"`
	UploadID string `json:"upload_id"`
	// Outcome is "stored" when this upload's content was stored, or
	// "aliased" when it was already stored and the upload was recorded as
	// an alias of the evidence holding it.
	Outcome    string       `json:"outcome"`
	IndexState string       `json:"index_state,omitempty"`
	Warning    string       `json:"warning,omitempty"`
	EviFile    *webBaseFile `json:"evi_file"`
}

type webBaseFile struct {
	FilePath          string           `json:"file_path"`
	FileId            string           `json:"file_id"`
	FileSize          int64            `json:"file_size"`
	ChunkMap          map[string]int64 `json:"chunk_map"`
	ChunkMapTruncated bool             `json:"chunk_map_truncated,omitempty"`
}

// uploads holds the server's upload sessions.
type uploads struct {
	s       *Server
	dir     string
	lim     UploadLimits
	hasher  func() hash.Hash
	hashLen int

	mu       sync.Mutex
	sessions map[string]*session
	ingest   chan struct{} // one ingest at a time

	// Seams for tests.
	now       func() time.Time
	free      func(string) (uint64, error)
	writeAt   func(f *os.File, p []byte, off int64) (int, error)
	runIngest func(context.Context, *badger.DB, ingest.Request) (ingest.Result, error)

	reaperDone chan struct{}
}

// newUploads prepares <root>/uploads and removes what earlier runs left
// there: a session lives only as long as its process.
func newUploads(s *Server) (*uploads, error) {
	if s.deps.Root == "" {
		return nil, errors.New("server: no data root")
	}
	lim := s.cfg.Limits.Upload
	if lim == (UploadLimits{}) {
		lim = DefaultUploadLimits()
	}
	m := &uploads{
		s:        s,
		dir:      filepath.Join(s.deps.Root, cnst.UploadsDir),
		lim:      lim,
		hasher:   dbmeta.FileHasher(s.deps.DB),
		sessions: map[string]*session{},
		ingest:   make(chan struct{}, 1),
		now:      time.Now,
		free:     diskspace.Free,
		writeAt:  func(f *os.File, p []byte, off int64) (int, error) { return f.WriteAt(p, off) },
		runIngest: func(ctx context.Context, db *badger.DB, req ingest.Request) (ingest.Result, error) {
			return ingest.Run(ctx, db, req)
		},
	}
	m.hashLen = m.hasher().Size()
	if err := os.MkdirAll(m.dir, 0o700); err != nil {
		return nil, fmt.Errorf("server: uploads directory: %w", err)
	}
	entries, err := os.ReadDir(m.dir)
	if err != nil {
		return nil, fmt.Errorf("server: uploads directory: %w", err)
	}
	removed := 0
	for _, e := range entries {
		name := e.Name()
		if !e.Type().IsRegular() {
			continue
		}
		// <id>.part, or a bare UUID: an upload of an earlier version.
		if _, err := uuid.Parse(strings.TrimSuffix(name, ".part")); err != nil {
			continue
		}
		if err := os.Remove(filepath.Join(m.dir, name)); err == nil {
			removed++
		}
	}
	if removed > 0 {
		s.Notes = append(s.Notes, fmt.Sprintf("removed %d unfinished uploads left by an earlier run", removed))
	}
	return m, nil
}

// startReaper ends idle and expired sessions until ctx ends.
func (m *uploads) startReaper(ctx context.Context) {
	m.reaperDone = make(chan struct{})
	go func() {
		defer close(m.reaperDone)
		t := time.NewTicker(m.lim.ReapInterval)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				m.reap()
			case <-ctx.Done():
				return
			}
		}
	}()
}

// reap ends sessions idle for IdleTTL or older than MaxLifetime, and forgets
// finished ones older than DoneTTL. A session with a chunk being written is
// left for the next round.
func (m *uploads) reap() {
	now := m.now()
	m.mu.Lock()
	var expired []*session
	for id, s := range m.sessions {
		switch s.state {
		case stateReceiving:
			if m.expired(s, now) {
				expired = append(expired, s)
			}
		case stateDone, stateFailed:
			if now.Sub(s.finished) > m.lim.DoneTTL {
				delete(m.sessions, id)
			}
		}
	}
	m.mu.Unlock()
	for _, s := range expired {
		if !s.tryLock() {
			continue
		}
		m.mu.Lock()
		gone := s.state == stateReceiving && m.expired(s, m.now())
		if gone {
			delete(m.sessions, s.id)
		}
		m.mu.Unlock()
		if gone {
			m.s.log.Info("upload expired", zap.String("upload_id", s.id), zap.Int64("offset", s.offset))
			m.dropFile(s)
		}
		s.unlock()
	}
}

func (m *uploads) expired(s *session, now time.Time) bool {
	return now.Sub(s.last) > m.lim.IdleTTL || now.Sub(s.created) > m.lim.MaxLifetime
}

// dropFile closes and removes a session's part file.
func (m *uploads) dropFile(s *session) {
	if s.f != nil {
		_ = s.f.Close()
		s.f = nil
	}
	if err := os.Remove(s.path); err != nil && !errors.Is(err, os.ErrNotExist) {
		m.s.log.Warn("upload file not removed", zap.String("path", s.path), zap.Error(err))
	}
}

// stop removes the part files of sessions still receiving. It runs once the
// server's requests and work have stopped.
func (m *uploads) stop() {
	if m.reaperDone != nil {
		<-m.reaperDone
	}
	m.mu.Lock()
	var open []*session
	for id, s := range m.sessions {
		if s.state == stateReceiving {
			open = append(open, s)
			delete(m.sessions, id)
		}
	}
	m.mu.Unlock()
	for _, s := range open {
		m.dropFile(s)
	}
}

// fileType checks a client's evidence type: short, lower case, and plain.
func fileType(t string) (string, error) {
	t = strings.ToLower(strings.TrimSpace(t))
	if t == cnst.UnknownEvidenceType {
		t = ""
	}
	if len(t) > 64 {
		return "", errors.New("file_type is longer than 64 characters")
	}
	for _, c := range t {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '_' || c == '-' || c == '.') {
			return "", errors.New("file_type may hold only a-z, 0-9, '_', '-' and '.'")
		}
	}
	return t, nil
}

// checkHash checks a client's file hash: base64 of a hash of the database's
// file hash algorithm.
func (m *uploads) checkHash(h string) (string, error) {
	h = strings.TrimSpace(h)
	if h == "" {
		return "", errors.New("file_hash is required")
	}
	b, err := base64.StdEncoding.DecodeString(h)
	if err != nil || len(b) != m.hashLen {
		return "", fmt.Errorf("file_hash must be the base64 of the file's %d-byte %s hash", m.hashLen, dbmeta.HashAlgo(m.s.deps.DB))
	}
	return h, nil
}

// start opens a session for a file of size bytes (-1: not declared).
func (m *uploads) start(path, ftype, h string, size int64) (*session, *uploadError) {
	if err := checkClientPath(path); err != nil {
		return nil, uploadErr(http.StatusBadRequest, "%v", err)
	}
	ft, err := fileType(ftype)
	if err != nil {
		return nil, uploadErr(http.StatusBadRequest, "%v", err)
	}
	if h, err = m.checkHash(h); err != nil {
		return nil, uploadErr(http.StatusBadRequest, "%v", err)
	}
	if size > m.lim.MaxSize {
		return nil, uploadErr(http.StatusRequestEntityTooLarge, "size %d is over the %d bytes one upload may hold", size, m.lim.MaxSize)
	}
	if m.s.draining.Load() {
		return nil, uploadErr(http.StatusServiceUnavailable, "the server is shutting down")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	active := 0
	for _, s := range m.sessions {
		if s.state == stateReceiving || s.state == stateFinalizing {
			active++
		}
	}
	if active >= m.lim.MaxSessions {
		return nil, uploadErr(http.StatusTooManyRequests, "%d uploads are already in progress; retry when one ends", active)
	}
	if size > 0 {
		free, err := m.free(m.dir)
		if err == nil && free < uint64(size+m.lim.Headroom) {
			return nil, uploadErr(http.StatusInsufficientStorage, "%d bytes are free for uploads; this one needs %d", free, size+m.lim.Headroom)
		}
	}
	id := uuid.NewString()
	part := filepath.Join(m.dir, id+".part")
	f, err := os.OpenFile(part, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		m.s.log.Error("upload file not created", zap.String("path", part), zap.Error(err))
		return nil, uploadErr(http.StatusInternalServerError, "the upload file could not be created")
	}
	now := m.now()
	s := &session{
		id: id, path: part, f: f, clientPath: path, fileType: ft, hash: h, sum: m.hasher(), size: size,
		created: now, last: now, state: stateReceiving,
		op: make(chan struct{}, 1), done: make(chan struct{}),
	}
	m.sessions[id] = s
	m.s.log.Info("upload started", zap.String("upload_id", id), zap.String("client_path", truncate(path, 512)),
		zap.Int64("size", size))
	return s, nil
}

func (m *uploads) get(id string) *session {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.sessions[id]
}

// write puts p at off (-1: at the end) of a session whose operation lock
// the caller holds, and returns the offset the session then holds. A retry
// of bytes already received writes nothing. A failed write leaves the
// session as it was.
func (m *uploads) write(s *session, off int64, p []byte) (int64, *uploadError) {
	m.mu.Lock()
	state, cur := s.state, s.offset
	m.mu.Unlock()
	if state != stateReceiving {
		return cur, uploadErr(http.StatusConflict, "the upload is %s", state)
	}
	if off < 0 {
		off = cur
	}
	n := int64(len(p))
	switch {
	case off == cur:
	case off < cur && off+n <= cur:
		// Bytes it already has: the client did not see the answer.
		return cur, nil
	default:
		return cur, &uploadError{status: http.StatusConflict, msg: fmt.Sprintf("the upload holds %d bytes; send from there", cur), offset: cur}
	}
	if s.size >= 0 && cur+n > s.size {
		return cur, &uploadError{status: http.StatusRequestEntityTooLarge, msg: fmt.Sprintf("the chunk goes past the declared size of %d bytes", s.size), offset: cur}
	}
	// Without a declared size, nothing was checked at start: the cap on one
	// upload applies to what arrives, so a stream cannot fill the disk the
	// database is on.
	if s.size < 0 && cur+n > m.lim.MaxSize {
		return cur, &uploadError{status: http.StatusRequestEntityTooLarge, msg: fmt.Sprintf("the upload goes past the %d bytes one upload may hold", m.lim.MaxSize), offset: cur}
	}
	if n == 0 {
		return cur, nil
	}
	w, err := m.writeAt(s.f, p, cur)
	if err == nil && int64(w) != n {
		err = io.ErrShortWrite
	}
	if err != nil {
		if terr := s.f.Truncate(cur); terr != nil {
			m.s.log.Error("upload file not truncated after a failed write", zap.String("upload_id", s.id), zap.Error(terr))
		}
		if diskspace.IsNoSpace(err) {
			return cur, &uploadError{status: http.StatusInsufficientStorage, msg: "no space is left for the upload", offset: cur}
		}
		m.s.log.Error("upload write failed", zap.String("upload_id", s.id), zap.Int64("offset", cur), zap.Error(err))
		return cur, &uploadError{status: http.StatusInternalServerError, msg: "the chunk could not be written", offset: cur}
	}
	s.sum.Write(p)
	m.mu.Lock()
	s.offset = cur + n
	s.last = m.now()
	m.mu.Unlock()
	return cur + n, nil
}

// finalize finalizes s, once: later calls wait for the first one's result.
// It waits up to wait (0: until done or ctx ends) and reports pending when
// the result is not there yet.
func (m *uploads) finalize(ctx context.Context, s *session, wait time.Duration) (res *uploadResult, uerr *uploadError, pending bool) {
	if err := s.lock(ctx); err != nil {
		return nil, nil, true
	}
	m.mu.Lock()
	if s.state == stateReceiving {
		if s.size >= 0 && s.offset != s.size {
			off, size := s.offset, s.size
			m.mu.Unlock()
			s.unlock()
			return nil, &uploadError{status: http.StatusConflict, msg: fmt.Sprintf("the upload holds %d of its %d bytes", off, size), offset: off}, false
		}
		s.state = stateFinalizing
		m.s.goWork(func(ctx context.Context) { m.runFinalize(ctx, s) })
	}
	m.mu.Unlock()
	s.unlock()

	var timeout <-chan time.Time
	if wait > 0 {
		t := time.NewTimer(wait)
		defer t.Stop()
		timeout = t.C
	}
	select {
	case <-s.done:
		m.mu.Lock()
		defer m.mu.Unlock()
		return s.result, s.err, false
	case <-timeout:
	case <-ctx.Done():
	}
	return nil, nil, true
}

func (m *uploads) runFinalize(ctx context.Context, s *session) {
	defer close(s.done)
	res, uerr := m.ingestUpload(ctx, s)
	m.mu.Lock()
	s.result, s.err = res, uerr
	s.state = stateDone
	if uerr != nil {
		s.state = stateFailed
	}
	s.finished = m.now()
	m.mu.Unlock()
	m.dropFile(s)
	if uerr != nil {
		m.s.log.Warn("upload failed", zap.String("upload_id", s.id), zap.Int("status", uerr.status), zap.String("error", uerr.msg))
		return
	}
	m.s.log.Info("upload stored", zap.String("upload_id", s.id), zap.String("outcome", res.Outcome), zap.String("evidence", res.EviFile.FileId))
}

// ingestUpload checks a received upload against its hash and stores it.
func (m *uploads) ingestUpload(ctx context.Context, s *session) (*uploadResult, *uploadError) {
	internal := func(what string, err error) *uploadError {
		m.s.log.Error("upload finalize failed", zap.String("upload_id", s.id), zap.String("op", what), zap.Error(err))
		return uploadErr(http.StatusInternalServerError, "the upload could not be stored (upload %s)", s.id)
	}
	err := s.f.Sync()
	if cerr := s.f.Close(); err == nil {
		err = cerr
	}
	s.f = nil
	if err != nil {
		return nil, internal("close", err)
	}
	if got := base64.StdEncoding.EncodeToString(s.sum.Sum(nil)); got != s.hash {
		m.s.log.Warn("upload hash mismatch", zap.String("upload_id", s.id), zap.String("client_hash", s.hash), zap.String("hash", got))
		return nil, uploadErr(http.StatusUnprocessableEntity, "the uploaded bytes do not match file_hash: their %s hash is %s", dbmeta.HashAlgo(m.s.deps.DB), got)
	}
	select {
	case m.ingest <- struct{}{}:
		defer func() { <-m.ingest }()
	case <-ctx.Done():
		return nil, uploadErr(http.StatusServiceUnavailable, "the server stopped before the upload was stored; upload it again")
	}
	db := m.s.deps.DB
	if err := faultinject.Hit(faultinject.UploadBeforeIngest); err != nil {
		return nil, internal("ingest", err)
	}
	res, err := m.runIngest(ctx, db, ingest.Request{
		Path:       s.path,
		Name:       clientBase(s.clientPath),
		SourcePath: s.clientPath,
		Decode:     true,
		Quiet:      true,
	})
	if err == nil {
		err = faultinject.Hit(faultinject.UploadAfterIngest)
	}
	var warning string
	var incomplete *ingest.IndexIncompleteError
	var mismatch *ingest.DigestMismatchError
	switch {
	case err == nil:
	case errors.As(err, &mismatch) && mismatch.Stored, errors.As(err, &incomplete):
		warning = err.Error()
	case ctx.Err() != nil:
		return nil, uploadErr(http.StatusServiceUnavailable, "the server stopped before the upload was stored; upload it again")
	case diskspace.IsNoSpace(err):
		return nil, uploadErr(http.StatusInsufficientStorage, "no space is left to store the upload")
	default:
		return nil, internal("ingest", err)
	}
	key := dbio.CanonicalEvidenceKey(res.EvidenceID)
	// The type the client gave is set only on evidence this upload stored.
	if res.Outcome == store.CommitStored && s.fileType != "" {
		err := dbio.UpdateEvidence(db, res.EvidenceID, func(_ *badger.Txn, e *structs.EvidenceFile) error {
			if e.EvidenceType == s.fileType {
				return dbio.ErrNoChange
			}
			e.EvidenceType = s.fileType
			return nil
		})
		if err != nil {
			return nil, internal("set type", err)
		}
	}
	e, err := dbio.GetEvidenceFile(key, db)
	if err != nil {
		return nil, internal("read evidence", err)
	}
	cm, truncated, err := chunkMap(ctx, db, key, chunkEntriesPerResponse)
	if err != nil {
		return nil, internal("chunk map", err)
	}
	out := &uploadResult{
		Done: true, UploadID: s.id, Outcome: string(res.Outcome), Warning: warning,
		EviFile: &webBaseFile{FilePath: s.clientPath, FileId: base64.StdEncoding.EncodeToString(key), FileSize: e.Size, ChunkMap: cm, ChunkMapTruncated: truncated},
	}
	if e.Index != nil {
		out.IndexState = string(e.Index.State)
	}
	return out, nil
}

// discard ends a session still receiving: its client went away.
func (m *uploads) discard(s *session) {
	if !s.tryLock() {
		return
	}
	defer s.unlock()
	m.mu.Lock()
	receiving := s.state == stateReceiving
	if receiving {
		delete(m.sessions, s.id)
	}
	m.mu.Unlock()
	if receiving {
		m.dropFile(s)
	}
}

// touch counts a status call as activity: a client asking is still there.
func (m *uploads) touch(s *session) {
	m.mu.Lock()
	if s.state == stateReceiving {
		s.last = m.now()
	}
	m.mu.Unlock()
}

// uploadStatus is what status says about a session.
type uploadStatus struct {
	UploadID  string        `json:"upload_id"`
	State     string        `json:"state"`
	Offset    int64         `json:"offset"`
	Size      *int64        `json:"size,omitempty"`
	ExpiresAt time.Time     `json:"expires_at"`
	Result    *uploadResult `json:"result,omitempty"`
	Error     string        `json:"error,omitempty"`
}

func (m *uploads) status(s *session) uploadStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	st := uploadStatus{UploadID: s.id, State: s.state, Offset: s.offset, Result: s.result}
	if s.size >= 0 {
		size := s.size
		st.Size = &size
	}
	if s.err != nil {
		st.Error = s.err.msg
	}
	switch s.state {
	case stateReceiving:
		st.ExpiresAt = s.last.Add(m.lim.IdleTTL)
		if end := s.created.Add(m.lim.MaxLifetime); end.Before(st.ExpiresAt) {
			st.ExpiresAt = end
		}
	case stateDone, stateFailed:
		st.ExpiresAt = s.finished.Add(m.lim.DoneTTL)
	}
	st.ExpiresAt = st.ExpiresAt.UTC()
	return st
}

// HTTP endpoints.

func (s *Server) uploadRoutes() {
	js := route{maxBody: s.cfg.Limits.MaxJSONBody, read: 30 * time.Second, write: 30 * time.Second}
	s.handle(s.mux, "POST /web/upload/start", js, http.HandlerFunc(s.handleUploadStart))
	s.handle(s.mux, "POST /web/upload/chunk", route{maxBody: maxChunk, idleRead: time.Minute, write: time.Minute}, http.HandlerFunc(s.handleUploadChunk))
	s.handle(s.mux, "POST /web/upload/status", js, http.HandlerFunc(s.handleUploadStatus))
	s.handle(s.mux, "POST /web/upload/abort", js, http.HandlerFunc(s.handleUploadAbort))
	// A finalize answers once FinalizeWait has passed, at the latest.
	fin := route{maxBody: s.cfg.Limits.MaxJSONBody, read: 30 * time.Second, write: s.uploads.lim.FinalizeWait + 30*time.Second}
	s.handle(s.mux, "POST /web/upload/finalize", fin, http.HandlerFunc(s.handleUploadFinalize))
}

func (s *Server) stopUploads() {
	if s.uploads != nil {
		s.uploads.stop()
	}
}

func writeUploadError(w http.ResponseWriter, r *http.Request, e *uploadError) {
	body := struct {
		errorBody
		Offset *int64 `json:"offset,omitempty"`
	}{errorBody: errorBody{Error: e.msg, RequestID: RequestID(r.Context())}}
	if e.offset >= 0 {
		off := e.offset
		body.Offset = &off
		w.Header().Set("Upload-Offset", strconv.FormatInt(off, 10))
	}
	if e.status == http.StatusTooManyRequests {
		w.Header().Set("Retry-After", "30")
	}
	writeJSON(w, e.status, body)
}

// session finds the session a request names, or answers 404.
func (s *Server) session(w http.ResponseWriter, r *http.Request, id string) *session {
	if id == "" {
		httpError(w, r, http.StatusBadRequest, "upload_id is required")
		return nil
	}
	sess := s.uploads.get(id)
	if sess == nil {
		httpError(w, r, http.StatusNotFound, "upload session not found or expired")
	}
	return sess
}

// handleUploadStart opens a session.
//
//	POST /web/upload/start
//	{"file_path": "...", "file_type": "...", "file_hash": "<base64>", "size": N}
//	→ {"upload_id": "...", "offset": 0, "size": N, "chunk_max": ..., "expires_at": "..."}
//
// size is required: the server checks it against its cap and its free space
// before taking any bytes.
func (s *Server) handleUploadStart(w http.ResponseWriter, r *http.Request) {
	var req struct {
		FilePath string `json:"file_path"`
		FileType string `json:"file_type"`
		FileHash string `json:"file_hash"`
		Size     *int64 `json:"size"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	switch {
	case req.Size == nil:
		// Upload v1 started without a size; it is no longer served.
		httpError(w, r, http.StatusBadRequest, "size is required: uploads without a declared size are not served")
		return
	case *req.Size < 0:
		httpError(w, r, http.StatusBadRequest, "size must not be negative")
		return
	}
	sess, uerr := s.uploads.start(req.FilePath, req.FileType, req.FileHash, *req.Size)
	if uerr != nil {
		writeUploadError(w, r, uerr)
		return
	}
	st := s.uploads.status(sess)
	writeJSON(w, http.StatusOK, struct {
		UploadID  string    `json:"upload_id"`
		Offset    int64     `json:"offset"`
		Size      int64     `json:"size"`
		ChunkMax  int       `json:"chunk_max"`
		ExpiresAt time.Time `json:"expires_at"`
	}{sess.id, 0, sess.size, maxChunk, st.ExpiresAt})
}

// handleUploadChunk writes one chunk.
//
//	POST /web/upload/chunk
//	Upload-Id: <id>
//	Upload-Offset: <where the chunk goes>
//	<the chunk's bytes, at most chunk_max>
//	→ {"ok": true, "offset": <bytes held>}, and Upload-Offset
//
// An offset other than what the session holds is 409 with the offset it
// holds, unless the chunk is one it already has, which is 200.
func (s *Server) handleUploadChunk(w http.ResponseWriter, r *http.Request) {
	sess := s.session(w, r, r.Header.Get("Upload-Id"))
	if sess == nil {
		return
	}
	h := r.Header.Get("Upload-Offset")
	if h == "" {
		httpError(w, r, http.StatusBadRequest, "Upload-Offset is required")
		return
	}
	off, err := strconv.ParseInt(h, 10, 64)
	if err != nil || off < 0 {
		httpError(w, r, http.StatusBadRequest, "Upload-Offset must be a byte offset")
		return
	}
	body, err := io.ReadAll(r.Body)
	var tooBig *http.MaxBytesError
	switch {
	case errors.As(err, &tooBig):
		httpError(w, r, http.StatusRequestEntityTooLarge, fmt.Sprintf("a chunk may hold at most %d bytes", maxChunk))
		return
	case err != nil:
		httpError(w, r, http.StatusBadRequest, "the chunk did not arrive whole")
		return
	}
	if err := sess.lock(r.Context()); err != nil {
		return
	}
	held, uerr := s.uploads.write(sess, off, body)
	sess.unlock()
	if uerr != nil {
		writeUploadError(w, r, uerr)
		return
	}
	w.Header().Set("Upload-Offset", strconv.FormatInt(held, 10))
	writeJSON(w, http.StatusOK, struct {
		OK     bool  `json:"ok"`
		Offset int64 `json:"offset"`
	}{true, held})
}

type uploadIDReq struct {
	UploadID string `json:"upload_id"`
}

// handleUploadStatus says where a session is.
//
//	POST /web/upload/status {"upload_id": "..."} → uploadStatus
func (s *Server) handleUploadStatus(w http.ResponseWriter, r *http.Request) {
	var req uploadIDReq
	if !decodeJSON(w, r, &req) {
		return
	}
	if sess := s.session(w, r, req.UploadID); sess != nil {
		s.uploads.touch(sess)
		writeJSON(w, http.StatusOK, s.uploads.status(sess))
	}
}

// handleUploadAbort ends a session and removes what it received. A session
// being finalized cannot be aborted.
//
//	POST /web/upload/abort {"upload_id": "..."} → {"ok": true}
func (s *Server) handleUploadAbort(w http.ResponseWriter, r *http.Request) {
	var req uploadIDReq
	if !decodeJSON(w, r, &req) {
		return
	}
	sess := s.session(w, r, req.UploadID)
	if sess == nil {
		return
	}
	if err := sess.lock(r.Context()); err != nil {
		return
	}
	defer sess.unlock()
	m := s.uploads
	m.mu.Lock()
	state := sess.state
	if state != stateFinalizing {
		delete(m.sessions, sess.id)
	}
	m.mu.Unlock()
	switch state {
	case stateFinalizing:
		writeUploadError(w, r, uploadErr(http.StatusConflict, "the upload is being finalized"))
		return
	case stateReceiving:
		m.dropFile(sess)
		s.log.Info("upload aborted", zap.String("upload_id", sess.id))
	}
	writeJSON(w, http.StatusOK, struct {
		OK bool `json:"ok"`
	}{true})
}

// handleUploadFinalize stores a session's file, once. The session must hold
// every declared byte. The answer is the result, or 202 with the session's
// status when the ingest takes longer than FinalizeWait; calling again, or
// status, gives the result.
//
//	POST /web/upload/finalize {"upload_id": "..."} → uploadResult
func (s *Server) handleUploadFinalize(w http.ResponseWriter, r *http.Request) {
	var req uploadIDReq
	if !decodeJSON(w, r, &req) {
		return
	}
	sess := s.session(w, r, req.UploadID)
	if sess == nil {
		return
	}
	res, uerr, pending := s.uploads.finalize(r.Context(), sess, s.uploads.lim.FinalizeWait)
	switch {
	case pending:
		writeJSON(w, http.StatusAccepted, s.uploads.status(sess))
	case uerr != nil:
		writeUploadError(w, r, uerr)
	default:
		writeJSON(w, http.StatusOK, res)
	}
}

// streamUpload receives a StreamFile upload: the first message carries the
// file's metadata, the rest its bytes in order. A client that goes away
// leaves no part file and no evidence (SRV-4).
func (s *Server) streamUpload(ctx context.Context, stream *connect.ClientStream[pb.StreamFileReq]) (*pb.StreamFileRes, error) {
	if !stream.Receive() {
		if err := stream.Err(); err != nil {
			return nil, err
		}
		return nil, invalidArg("the stream is empty")
	}
	meta := stream.Msg().GetFileMeta()
	if meta == nil {
		return nil, invalidArg("the first message must carry file_meta")
	}
	size := meta.GetSize()
	if size <= 0 {
		size = -1
	}
	sess, uerr := s.uploads.start(meta.GetFilePath(), meta.GetFileType(), meta.GetFileHash(), size)
	if uerr != nil {
		return nil, uploadRPCError(uerr)
	}
	defer s.uploads.discard(sess)
	for stream.Receive() {
		if stream.Msg().GetFileMeta() != nil {
			return nil, invalidArg("file_meta may come only in the first message")
		}
		if err := sess.lock(ctx); err != nil {
			return nil, err
		}
		_, uerr := s.uploads.write(sess, -1, stream.Msg().GetFile())
		sess.unlock()
		if uerr != nil {
			return nil, uploadRPCError(uerr)
		}
	}
	if err := stream.Err(); err != nil {
		return nil, err
	}
	res, uerr, pending := s.uploads.finalize(ctx, sess, 0)
	switch {
	case pending:
		return nil, ctx.Err()
	case uerr != nil:
		return nil, uploadRPCError(uerr)
	}
	f := res.EviFile
	return &pb.StreamFileRes{Done: true, EviFile: &pb.BaseFile{
		FilePath: f.FilePath, FileId: f.FileId, FileSize: f.FileSize, ChunkMap: f.ChunkMap, ChunkMapTruncated: f.ChunkMapTruncated,
	}}, nil
}

// uploadRPCError is an upload refusal as a Connect error.
func uploadRPCError(e *uploadError) error {
	code := connect.CodeInternal
	switch e.status {
	case http.StatusBadRequest:
		code = connect.CodeInvalidArgument
	case http.StatusNotFound:
		code = connect.CodeNotFound
	case http.StatusConflict:
		code = connect.CodeFailedPrecondition
	case http.StatusRequestEntityTooLarge:
		code = connect.CodeOutOfRange
	case http.StatusUnprocessableEntity:
		code = connect.CodeDataLoss
	case http.StatusTooManyRequests, http.StatusInsufficientStorage:
		code = connect.CodeResourceExhausted
	case http.StatusServiceUnavailable:
		code = connect.CodeUnavailable
	}
	return connect.NewError(code, errors.New(e.msg))
}

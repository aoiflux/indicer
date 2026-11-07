package server

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"indicer/lib/cnst"
	"indicer/lib/dbio"
	"indicer/lib/search"
	"indicer/lib/structs"
	"indicer/pb"

	"connectrpc.com/connect"
	"github.com/dgraph-io/badger/v4"
)

// GetIdxFiles lists a partition's indexed files in member order. The
// partition of an index quarantined as wrong lists none.
func (r *rpcService) GetIdxFiles(ctx context.Context, req *connect.Request[pb.GetIdxFilesReq]) (*connect.Response[pb.GetIdxFilesRes], error) {
	id := req.Msg.GetPartiFileId()
	if id == "" {
		return nil, invalidArg("parti_file_id is required")
	}
	pg, err := readPage(req.Msg.GetPageSize(), req.Msg.GetPageToken())
	if err != nil {
		return nil, err
	}
	raw, err := base64.StdEncoding.DecodeString(id)
	if err != nil || len(raw) == 0 {
		return nil, invalidArg("parti_file_id is not a base64 record ID")
	}
	pkey := dbio.CanonicalPartitionKey(raw)
	p, err := dbio.GetPartitionFile(pkey, r.db())
	if errors.Is(err, badger.ErrKeyNotFound) {
		return nil, notFound("no partition %s", id)
	}
	if err != nil {
		return nil, err
	}
	eid, err := dbio.RecordEvidenceID(p.IndexedFile)
	if err != nil {
		return nil, fmt.Errorf("partition %s: %w", id, err)
	}
	e, err := dbio.GetEvidenceFile(dbio.CanonicalEvidenceKey(eid), r.db())
	if errors.Is(err, badger.ErrKeyNotFound) || (err == nil && !completed(e)) {
		return nil, notFound("no partition %s of stored evidence", id)
	}
	if err != nil {
		return nil, err
	}
	res := &pb.GetIdxFilesRes{Done: true}
	if quarantined(e) {
		return connect.NewResponse(res), nil
	}
	ids, err := memberPage(ctx, r.db(), pkey, p, string(pg.after), pg.n+1)
	if err != nil {
		return nil, err
	}
	b := &chunkBudget{left: chunkEntriesPerResponse}
	last := ""
	for _, m := range ids {
		if len(res.IndexedFile) == pg.n {
			res.NextPageToken = pageToken([]byte(last))
			break
		}
		rawID, err := base64.StdEncoding.DecodeString(m)
		if err != nil {
			return nil, fmt.Errorf("partition member %q: %w", m, err)
		}
		key := dbio.CanonicalIndexedKey(rawID)
		rec, err := dbio.GetIndexedFile(key, r.db())
		if errors.Is(err, badger.ErrKeyNotFound) {
			// A member written before its record, by a pass that died.
			last = m
			continue
		}
		if err != nil {
			return nil, err
		}
		bf, ok, err := r.baseFile(ctx, key, dbio.RecordPath(rec), rec.Size, b, len(res.IndexedFile) == 0)
		if err != nil {
			return nil, err
		}
		if !ok {
			res.NextPageToken = pageToken([]byte(last))
			break
		}
		res.IndexedFile = append(res.IndexedFile, bf)
		last = m
	}
	return connect.NewResponse(res), nil
}

// memberPage returns up to n member IDs (base64) of the partition pkey that
// sort after after: the legacy map's and the member keys', merged in order.
func memberPage(ctx context.Context, db *badger.DB, pkey []byte, p structs.PartitionFile, after string, n int) ([]string, error) {
	var legacy []string
	for id := range p.InternalObjects {
		if id > after {
			legacy = append(legacy, id)
		}
	}
	sort.Strings(legacy)
	var keyed []string
	if p.MemberCount > 0 {
		err := db.View(func(txn *badger.Txn) error {
			prefix := dbio.PartitionMemberPrefix(pkey)
			opts := badger.DefaultIteratorOptions
			opts.Prefix = prefix
			opts.PrefetchValues = false
			it := txn.NewIterator(opts)
			defer it.Close()
			for it.Seek(append(append([]byte(nil), prefix...), after...)); it.Valid() && len(keyed) < n; it.Next() {
				if err := ctx.Err(); err != nil {
					return err
				}
				id := string(it.Item().Key()[len(prefix):])
				if id <= after {
					continue
				}
				if _, dup := p.InternalObjects[id]; !dup {
					keyed = append(keyed, id)
				}
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	out := make([]string, 0, min(n, len(legacy)+len(keyed)))
	for len(out) < n && (len(legacy) > 0 || len(keyed) > 0) {
		if len(keyed) == 0 || (len(legacy) > 0 && legacy[0] < keyed[0]) {
			out, legacy = append(out, legacy[0]), legacy[1:]
		} else {
			out, keyed = append(out, keyed[0]), keyed[1:]
		}
	}
	return out, nil
}

// evidenceByHash returns the stored evidence whose content has the hash h
// (base64): its content root, or a hash it was entered under (plain, or the
// file hash its strategy records). The ID is nil when there is none.
func evidenceByHash(db *badger.DB, h string) ([]byte, structs.EvidenceFile, error) {
	var id []byte
	var rec structs.EvidenceFile
	err := db.View(func(txn *badger.Txn) error {
		try := func(raw []byte) (bool, error) {
			e, err := dbio.GetEvidenceTxn(txn, raw)
			if errors.Is(err, badger.ErrKeyNotFound) {
				return false, nil
			}
			if err != nil || !completed(e) {
				return false, err
			}
			id, rec = append([]byte(nil), raw...), e
			return true, nil
		}
		item, err := txn.Get(dbio.RootLookupKey(h))
		switch {
		case err == nil:
			raw, err := item.ValueCopy(nil)
			if err != nil {
				return err
			}
			if ok, err := try(raw); ok || err != nil {
				return err
			}
		case !errors.Is(err, badger.ErrKeyNotFound):
			return err
		}
		members, err := dbio.HashLookupMembersTxn(txn, []byte(cnst.EviFileHashLookupNamespace+h))
		if errors.Is(err, badger.ErrKeyNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		for _, m := range members {
			if ok, err := try(dbio.EvidenceRawID(m)); ok || err != nil {
				return err
			}
		}
		return nil
	})
	return id, rec, err
}

// checkClientPath checks a path a client gives for a file.
func checkClientPath(p string) error {
	switch {
	case p == "":
		return errors.New("file_path is required")
	case len(p) > maxClientPath:
		return fmt.Errorf("file_path is longer than %d bytes", maxClientPath)
	case !utf8.ValidString(p) || strings.ContainsRune(p, 0):
		return errors.New("file_path is not valid UTF-8 text")
	}
	return nil
}

// clientBase is the last element of a client's path, whichever separator it
// uses.
func clientBase(p string) string {
	if i := strings.LastIndexAny(p, `/\`); i >= 0 && i < len(p)-1 {
		return p[i+1:]
	}
	return p
}

// recordAlias records path as an alias of the evidence id unless the
// evidence already goes by it. It never renames the evidence (SRV-5). It
// reports whether it recorded one.
func recordAlias(db *badger.DB, id []byte, e structs.EvidenceFile, path string) (bool, error) {
	if path == e.Name {
		return false, nil
	}
	added := false
	err := dbio.UpdateTxn(db, func(txn *badger.Txn) error {
		added = false
		key := dbio.AliasKey(id, path)
		switch _, err := txn.Get(key); {
		case err == nil:
			return dbio.ErrNoChange
		case !errors.Is(err, badger.ErrKeyNotFound):
			return err
		}
		added = true
		return dbio.PutRecordTxn(db, txn, key, structs.AliasRecord{
			Name:   clientBase(path),
			Path:   path,
			Size:   e.Size,
			Source: structs.EvidenceSource{Format: structs.SourceClient, Members: []structs.SourceMember{{Path: path, Size: e.Size}}},
			At:     time.Now().UnixNano(),
		})
	})
	if errors.Is(err, dbio.ErrNoChange) {
		err = nil
	}
	return added, err
}

// AppendIfExists tells a client whether content is already stored, so it
// need not upload it, and records the client's path as an alias of the
// evidence holding it. It used to look the hash up as an evidence key, which
// evidence keys have not been since they became UUIDs, and to rename the
// evidence it found (SRV-3, SRV-5).
func (r *rpcService) AppendIfExists(ctx context.Context, req *connect.Request[pb.AppendIfExistsReq]) (*connect.Response[pb.AppendIfExistsRes], error) {
	h := strings.TrimSpace(req.Msg.GetFileHash())
	if h == "" {
		return nil, invalidArg("file_hash is required")
	}
	if b, err := base64.StdEncoding.DecodeString(h); err != nil || len(b) == 0 {
		return nil, invalidArg("file_hash is not base64")
	}
	path := req.Msg.GetFilePath()
	if err := checkClientPath(path); err != nil {
		return nil, invalidArg("%v", err)
	}
	id, e, err := evidenceByHash(r.db(), h)
	if err != nil {
		return nil, err
	}
	res := &pb.AppendIfExistsRes{}
	if id == nil {
		return connect.NewResponse(res), nil
	}
	if res.Appended, err = recordAlias(r.db(), id, e, path); err != nil {
		return nil, err
	}
	res.Exists = true
	if res.EviFile, _, err = r.baseFile(ctx, dbio.CanonicalEvidenceKey(id), path, e.Size, &chunkBudget{left: chunkEntriesPerResponse}, true); err != nil {
		return nil, err
	}
	return connect.NewResponse(res), nil
}

// Search counts the keyword, taken as one literal term matched without
// regard to case, in every stored object, within searchBudget and with at
// most searchSlots searches at once.
func (r *rpcService) Search(ctx context.Context, req *connect.Request[pb.SearchReq]) (*connect.Response[pb.SearchRes], error) {
	q := strings.TrimSpace(req.Msg.GetKeyword())
	if q == "" {
		return nil, invalidArg("keyword is required")
	}
	select {
	case r.searches <- struct{}{}:
		defer func() { <-r.searches }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	sctx, cancel := context.WithTimeout(ctx, searchBudget)
	defer cancel()
	res, err := search.Run(sctx, r.db(), []string{q}, search.Options{})
	switch {
	case errors.Is(err, search.ErrQueryTooLong), errors.Is(err, cnst.ErrSmallQuery):
		return nil, invalidArg("%v", err)
	case errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil:
		return nil, connect.NewError(connect.CodeDeadlineExceeded, fmt.Errorf("search exceeded its %v time budget", searchBudget))
	case err != nil:
		return nil, err
	}
	hits := res.Hits
	out := &pb.SearchRes{TotalCount: res.LevelTotal(search.LevelEvidence, 0)}
	if len(hits) > maxSearchHits {
		hits = append([]search.Hit(nil), hits...)
		sort.SliceStable(hits, func(i, j int) bool { return hits[i].Counts[0] > hits[j].Counts[0] })
		hits, out.Truncated = hits[:maxSearchHits], true
	}
	out.KeywordCountMap = make(map[string]int64, len(hits))
	for _, h := range hits {
		out.KeywordCountMap[base64.StdEncoding.EncodeToString(h.Key)] = h.Counts[0]
	}
	return connect.NewResponse(out), nil
}

// StreamFile receives an upload as a client stream (see upload.go).
func (r *rpcService) StreamFile(ctx context.Context, stream *connect.ClientStream[pb.StreamFileReq]) (*connect.Response[pb.StreamFileRes], error) {
	res, err := r.s.streamUpload(ctx, stream)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(res), nil
}

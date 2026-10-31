package index

import (
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"sync/atomic"

	"indicer/lib/cnst"
	"indicer/lib/dbio"
	"indicer/lib/parser/core"
	"indicer/lib/parser/partition"
	"indicer/lib/store"
	"indicer/lib/structs"
	"indicer/lib/util"

	"github.com/dgraph-io/badger/v4"
)

// openPartition is the partition the scan is walking.
type openPartition struct {
	info  core.PartitionInfo
	id    []byte
	key   []byte // P|||<id>
	name  string // the record's name, in the legacy form
	label string // the display name: <evidence name>_p<slot>

	// batch holds the partition's indexed records. Hash workers write to it
	// too; badger's WriteBatch is safe for concurrent use.
	batch   *badger.WriteBatch
	pending sync.WaitGroup // hash jobs not yet written
	indexed atomic.Int64   // records written

	// seen holds the first 8 bytes of every ID given out, so two entries
	// that hash to the same ID (two deleted entries with one name and
	// inode) each get their own.
	seen map[uint64]struct{}
	rep  structs.PartitionReport
}

// beginPartition writes the partition's record and links it from the
// evidence before any member exists, so a pass that stops anywhere after
// leaves nothing a delete of the evidence's index cannot find.
func (ix *indexer) beginPartition(p core.PartitionInfo) error {
	if err := ix.failure(); err != nil {
		return err
	}
	id := PartitionID(ix.opts.EvidenceID, p.Index, p.Offset)
	label := partitionLabel(ix.opts.EvidenceName, p.Index)
	op := &openPartition{
		info:  p,
		id:    id,
		key:   dbio.CanonicalPartitionKey(id),
		name:  base64.StdEncoding.EncodeToString(ix.opts.EvidenceID) + cnst.DataSeperator + label,
		label: label,
		seen:  map[uint64]struct{}{},
	}
	if err := ix.putPartition(op, 0); err != nil {
		return fmt.Errorf("index: partition %s: %w", op.name, err)
	}
	err := dbio.UpdateEvidence(ix.opts.DB, ix.opts.EvidenceID, func(_ *badger.Txn, e *structs.EvidenceFile) error {
		idB64 := base64.StdEncoding.EncodeToString(id)
		want := structs.InternalOffset{Start: p.Offset, End: p.Offset + p.Size - 1}
		if got, ok := e.InternalObjects[idB64]; ok && got == want {
			return dbio.ErrNoChange
		}
		if e.InternalObjects == nil {
			e.InternalObjects = map[string]structs.InternalOffset{}
		}
		e.InternalObjects[idB64] = want
		return nil
	})
	if err != nil {
		return fmt.Errorf("index: link partition %s to its evidence: %w", op.name, err)
	}
	op.batch = ix.opts.DB.NewWriteBatch()
	ix.part = op
	ix.phase.Note(fmt.Sprintf("partition %d, %s", p.Index, p.FSType))
	ix.phase.Item("")
	ix.seen = 0
	ix.parts = append(ix.parts, Partition{ID: id, Ordinal: p.Index, Offset: p.Offset, Size: p.Size, FSType: p.FSType, Status: p.Status})
	return nil
}

// endPartition waits for the partition's files to be written, then writes its
// final record, which counts them, and its report.
func (ix *indexer) endPartition(p core.PartitionInfo, rep *core.IndexReport) error {
	op := ix.part
	if op == nil {
		return errors.New("index: a partition ended that never began")
	}
	op.pending.Wait()
	if err := ix.failure(); err != nil {
		return err
	}
	ix.part = nil
	if err := op.batch.Flush(); err != nil {
		return fmt.Errorf("index: partition %s: %w", op.name, err)
	}
	op.info = p
	ix.parts[len(ix.parts)-1].Status = p.Status
	ix.parts[len(ix.parts)-1].FSType = p.FSType
	switch p.Status {
	case core.PartPartial, core.PartOpenError:
		ix.problem("partition %s (%s) is %s: %s", op.name, p.FSType, p.Status, p.Detail)
	}

	op.rep.FilesIndexed = op.indexed.Load()
	ix.cov.FilesIndexed += op.rep.FilesIndexed
	if err := ix.putPartition(op, int(op.rep.FilesIndexed)); err != nil {
		return fmt.Errorf("index: partition %s: %w", op.name, err)
	}
	report := op.rep
	fillReport(&report, rep)
	err := ix.opts.DB.Update(func(txn *badger.Txn) error {
		return dbio.PutRecordTxn(txn, dbio.PartitionReportKey(op.id), report)
	})
	if err != nil {
		return fmt.Errorf("index: partition %s report: %w", op.name, err)
	}
	return nil
}

// putPartition writes the partition's record with members counted. A hash
// set on an earlier record is kept: it is the hash of the same range.
func (ix *indexer) putPartition(op *openPartition, members int) error {
	p := op.info
	typ := string(p.FSType)
	if p.FSType == "" || p.FSType == core.FSUnknown {
		typ = cnst.UnknownEvidenceType
	}
	rec := structs.NewPartitionFile(op.name, p.Offset, p.Size, nil, "")
	rec.IndexedType = typ
	rec.EvidenceID = ix.opts.EvidenceID
	rec.Path = op.label
	rec.MemberCount = members
	rec.Partition = &structs.PartitionMeta{
		Ordinal:  p.Index,
		TypeName: p.TypeName,
		Label:    p.Name,
		FSType:   string(p.FSType),
		Status:   string(p.Status),
		Detail:   p.Detail,
	}
	return dbio.UpdateTxn(ix.opts.DB, func(txn *badger.Txn) error {
		var old structs.PartitionFile
		switch err := dbio.GetRecordTxn(txn, op.key, &old); {
		case err == nil:
			rec.FileHash = old.FileHash
		case !errors.Is(err, badger.ErrKeyNotFound):
			return err
		}
		return dbio.PutRecordTxn(txn, op.key, rec)
	})
}

// partitionLabel names a partition by its table slot (a filesystem spanning
// the whole device is _dev). The record's Name is base64(evidence ID)|||label,
// the legacy form list associates partitions by.
func partitionLabel(evidence string, slot int) string {
	if slot < 0 {
		return evidence + "_dev"
	}
	return evidence + "_" + cnst.PartitionIndexPrefix + strconv.Itoa(slot)
}

func fillReport(dst *structs.PartitionReport, rep *core.IndexReport) {
	if rep == nil {
		return
	}
	dst.FilesSeen = rep.FilesSeen
	dst.DirsSeen = rep.DirsSeen
	dst.OtherSeen = rep.OtherSeen
	dst.Deleted = rep.Deleted
	dst.Fragmented = rep.Fragmented
	dst.Sparse = rep.Sparse
	dst.Inline = rep.Inline
	dst.Decoded = rep.Decoded
	dst.Unavailable = rep.Unavailable
	dst.Truncated = rep.Truncated
	dst.BytesData = rep.BytesData
	if len(rep.Warnings) == 0 {
		return
	}
	dst.Warnings = make(map[string]int64, len(rep.Warnings))
	dst.Samples = make(map[string][]structs.IssueSample, len(rep.Samples))
	for code, n := range rep.Warnings {
		dst.Warnings[string(code)] = n
	}
	for code, samples := range rep.Samples {
		out := make([]structs.IssueSample, len(samples))
		for i, s := range samples {
			out[i] = structs.IssueSample{Code: string(s.Code), Path: s.Path, Inode: s.Inode, Detail: s.Detail}
		}
		dst.Samples[string(code)] = out
	}
}

// SetPartitionHash records hash, the plain hash of partition pid's range, on
// its record and in the partition hash lookup. The chunk pass computes it as
// it stores the evidence (ING-12), so the orchestrator sets it once both
// passes are done.
func SetPartitionHash(db *badger.DB, pid, hash []byte) error {
	key := dbio.CanonicalPartitionKey(pid)
	enc := base64.StdEncoding.EncodeToString(hash)
	return dbio.UpdateTxn(db, func(txn *badger.Txn) error {
		var rec structs.PartitionFile
		if err := dbio.GetRecordTxn(txn, key, &rec); err != nil {
			return err
		}
		if rec.FileHash == enc {
			return dbio.ErrNoChange
		}
		if rec.FileHash != "" {
			old := util.AppendToBytesSlice(cnst.PartiFileHashLookupNamespace, rec.FileHash)
			if err := txn.Delete(dbio.HashLookupMemberKey(old, key)); err != nil {
				return err
			}
		}
		rec.FileHash = enc
		if err := dbio.PutRecordTxn(txn, key, rec); err != nil {
			return err
		}
		lookup := util.AppendToBytesSlice(cnst.PartiFileHashLookupNamespace, enc)
		return dbio.AppendHashLookupUUIDByKeyTxn(txn, lookup, key)
	})
}

// PartitionRanges returns the ranges of img a scan announces as partitions,
// so the chunk pass can hash them as it stores the evidence. A filesystem
// that spans the whole device is the evidence's own plain hash, and is not
// listed. A table that cannot be parsed yields no ranges.
func PartitionRanges(img core.Image) []store.ByteRange {
	table, _, err := partition.ParseReport(img)
	if err != nil {
		return nil
	}
	var out []store.ByteRange
	for _, p := range table.Partitions {
		if p.Allocated && p.Size > 0 {
			out = append(out, store.ByteRange{Offset: p.Offset, Length: p.Size})
		}
	}
	return out
}

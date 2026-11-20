package dbio

import (
	"crypto/sha256"
	"errors"
	"strconv"
	"time"

	"indicer/internal/msgpackx"
	"indicer/lib/cnst"
	"indicer/lib/engine"
	"indicer/lib/structs"
	"indicer/lib/util"
	"indicer/lib/zstdx"

	"github.com/dgraph-io/badger/v4"
	"github.com/vmihailenco/msgpack/v5"
)

// Keys of content identity (D1) and of the W6 records.

// RootLookupKey is ER|||:<root>, which names the one evidence holding the
// content whose hocho root is root (base64).
func RootLookupKey(root string) []byte {
	return util.AppendToBytesSlice(cnst.EviRootLookupNamespace, root)
}

// QuickLookupKey is EQ|||:<size>|||<fingerprint>, the hash lookup of evidence
// that may hold content of that size and fingerprint.
func QuickLookupKey(size int64, fingerprint string) []byte {
	return util.AppendToBytesSlice(cnst.EviQuickLookupNamespace, strconv.FormatInt(size, 10), cnst.DataSeperator, fingerprint)
}

// PathDigest is the digest alias and attempt keys use for a source path.
func PathDigest(path string) []byte {
	d := sha256.Sum256([]byte(path))
	return d[:]
}

// AliasKey is EA|||:<winner raw ID><sha256(path)>: one alias per evidence
// and source path, so storing the same file again rewrites the same record.
func AliasKey(winner []byte, path string) []byte {
	return util.AppendToBytesSlice(cnst.EviAliasNamespace, winner, PathDigest(path))
}

// AliasPrefix is the prefix of every alias of winner.
func AliasPrefix(winner []byte) []byte {
	return util.AppendToBytesSlice(cnst.EviAliasNamespace, winner)
}

// AttemptKey is EP|||:<sha256(path)>, naming the evidence an ingest of path
// started and has not finished.
func AttemptKey(path string) []byte {
	return util.AppendToBytesSlice(cnst.EviAttemptNamespace, PathDigest(path))
}

// Domains that keep merge keys apart from local ones: a NUL never appears in
// a path, so no AttemptKey or AliasKey input can start with one.
const (
	mergeAttemptDomain = "\x00dues-merge-attempt"
	mergeAliasDomain   = "\x00dues-merge-alias"
)

// MergeLedgerKey is EM|||:<source raw ID>|||<sha256(algo|||hash|||size)>: one
// entry per source evidence and content, so sources that reuse an ID never
// share one.
func MergeLedgerKey(m structs.MergeInfo, size int64) []byte {
	content := sha256.Sum256(util.AppendToBytesSlice(m.SourceHashAlgo, cnst.DataSeperator, m.SourceFileHash, cnst.DataSeperator, size))
	return util.AppendToBytesSlice(cnst.EviMergeNamespace, m.SourceEvidenceID, cnst.DataSeperator, content[:])
}

// MergeAttemptKey is the EP key of a merge attempt, never equal to the
// AttemptKey of any path.
func MergeAttemptKey(m structs.MergeInfo, size int64) []byte {
	d := sha256.Sum256(util.AppendToBytesSlice(mergeAttemptDomain, MergeLedgerKey(m, size)))
	return util.AppendToBytesSlice(cnst.EviAttemptNamespace, d[:])
}

// MergeAliasKey is the key of a merged alias of winner. It has AliasKey's
// length, so AliasPrefix scans find it, and never equals a local alias key, so
// local and merged aliases at one path never overwrite each other.
func MergeAliasKey(winner []byte, m structs.MergeInfo, size int64, path string) []byte {
	d := sha256.Sum256(util.AppendToBytesSlice(mergeAliasDomain, MergeLedgerKey(m, size), path))
	return util.AppendToBytesSlice(cnst.EviAliasNamespace, winner, d[:])
}

// ObjectKey is X|||:<object raw ID>, a materialised object's record.
func ObjectKey(oid []byte) []byte {
	return util.AppendToBytesSlice(cnst.ObjectNamespace, oid)
}

// ExtentPageKey is XE|||:<indexed raw ID>|||<page>.
func ExtentPageKey(iid []byte, page int) []byte {
	return util.AppendToBytesSlice(cnst.ExtentPageNamespace, iid, cnst.DataSeperator, strconv.Itoa(page))
}

// ExtentPagePrefix is the prefix of every extent page of the indexed file iid.
func ExtentPagePrefix(iid []byte) []byte {
	return util.AppendToBytesSlice(cnst.ExtentPageNamespace, iid, cnst.DataSeperator)
}

// PartitionReportKey is PR|||:<partition raw ID>.
func PartitionReportKey(pid []byte) []byte {
	return util.AppendToBytesSlice(cnst.PartitionReportNamespace, pid)
}

// RelationPrefix is the prefix of every relation of the object rid.
func RelationPrefix(rid []byte) []byte {
	return util.AppendToBytesSlice(cnst.RelationNamespace, rid, cnst.DataSeperator)
}

// PutRecordTxn writes v inside txn, a transaction on db, encoded the way
// every record of db is.
func PutRecordTxn(db *badger.DB, txn *badger.Txn, key []byte, v any) error {
	data, err := EncodeRecord(db, v)
	if err != nil {
		return err
	}
	return txn.Set(key, data)
}

// EncodeRecord encodes v the way SetFile encodes the records of db: msgpack,
// then db's value encoding.
func EncodeRecord(db *badger.DB, v any) ([]byte, error) {
	data, err := msgpack.Marshal(v)
	if err != nil {
		return nil, err
	}
	return engine.For(db).EncodeValue(data), nil
}

// GetRecordTxn reads the record at key into v. A missing key is
// badger.ErrKeyNotFound.
func GetRecordTxn(txn *badger.Txn, key []byte, v any) error {
	item, err := txn.Get(key)
	if err != nil {
		return err
	}
	data, err := item.ValueCopy(nil)
	if err != nil {
		return err
	}
	return DecodeRecord(data, v)
}

// DecodeRecord decodes a record value, compressed or not.
func DecodeRecord(data []byte, v any) error {
	if decoded, err := zstdx.DecodeAll(data, nil); err == nil {
		data = decoded
	}
	return msgpackx.Unmarshal(data, v)
}

// GetEvidenceTxn reads the evidence record eid inside txn.
func GetEvidenceTxn(txn *badger.Txn, eid []byte) (structs.EvidenceFile, error) {
	return getEvidenceTxn(txn, CanonicalEvidenceKey(eid))
}

// SetEvidenceTxn writes the evidence record eid inside txn, a transaction on
// db.
func SetEvidenceTxn(db *badger.DB, txn *badger.Txn, eid []byte, e structs.EvidenceFile) error {
	return setEvidenceTxn(db, txn, CanonicalEvidenceKey(eid), e)
}

// Aliases returns every alias of the evidence winner, in key order.
func Aliases(db *badger.DB, winner []byte) ([]structs.AliasRecord, error) {
	var out []structs.AliasRecord
	err := db.View(func(txn *badger.Txn) error {
		opts := badger.DefaultIteratorOptions
		opts.Prefix = AliasPrefix(winner)
		// The prefix has no delimiter: a longer ID that starts with winner
		// shares it, and only the key length tells its aliases apart.
		keyLen := len(opts.Prefix) + sha256.Size
		it := txn.NewIterator(opts)
		defer it.Close()
		for it.Rewind(); it.Valid(); it.Next() {
			if len(it.Item().Key()) != keyLen {
				continue
			}
			data, err := it.Item().ValueCopy(nil)
			if err != nil {
				return err
			}
			var a structs.AliasRecord
			if err := DecodeRecord(data, &a); err != nil {
				return err
			}
			out = append(out, a)
		}
		return nil
	})
	return out, err
}

// UpdateTxn runs fn in a read-write transaction, retried as UpdateEvidence
// retries when it conflicts with a concurrent write; fn may run more than
// once. fn returning ErrNoChange commits nothing and UpdateTxn returns nil.
func UpdateTxn(db *badger.DB, fn func(txn *badger.Txn) error) error {
	for attempt := 1; ; attempt++ {
		err := db.Update(fn)
		if errors.Is(err, ErrNoChange) {
			return nil
		}
		if !errors.Is(err, badger.ErrConflict) || attempt == maxEvidenceUpdateAttempts {
			return err
		}
		time.Sleep(conflictBackoff(attempt))
	}
}

// ReverseRelationAppendMemberKey is the reverse relation key recording that
// chunk chash occurs at index in the object fhash.
func ReverseRelationAppendMemberKey(chash []byte, index int64, fhash []byte) []byte {
	return buildReverseRelationAppendMemberKey(chash, index, reverseRelationAppendShard(fhash), fhash)
}

// FileSimhashKey is the cached file SimHash key of the record key fid.
func FileSimhashKey(fid []byte) []byte {
	return util.AppendToBytesSlice(cnst.FileSimhashNamespace, fid)
}

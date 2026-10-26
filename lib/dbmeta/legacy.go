package dbmeta

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"indicer/internal/msgpackx"
	"indicer/lib/blobcodec"
	"indicer/lib/cnst"
	"indicer/lib/structs"

	"github.com/dgraph-io/badger/v4"
)

// badger's key registry starts with an AES IV and a sanity text, encrypted
// with the database key when there is one (badger key_registry.go).
const keyRegistryName = "KEYREGISTRY"

var sanityText = []byte("Hello Badger")

func readRegistryHead(kvdb string) (iv, sanity []byte, err error) {
	f, err := os.Open(filepath.Join(kvdb, keyRegistryName))
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()
	head := make([]byte, aes.BlockSize+len(sanityText))
	if _, err := io.ReadFull(f, head); err != nil {
		return nil, nil, fmt.Errorf("reading %s: %w", keyRegistryName, err)
	}
	return head[:aes.BlockSize], head[aes.BlockSize:], nil
}

// ProbeLegacyEncryption reports whether the badger database in kvdb is
// encrypted, from its key registry, without opening it.
func ProbeLegacyEncryption(kvdb string) (bool, error) {
	_, sanity, err := readRegistryHead(kvdb)
	if err != nil {
		return false, err
	}
	return !bytes.Equal(sanity, sanityText), nil
}

// CheckLegacyKey reports whether key opens the badger database in kvdb. It
// returns ErrWrongPassword when it does not.
func CheckLegacyKey(kvdb string, key []byte) error {
	iv, sanity, err := readRegistryHead(kvdb)
	if err != nil {
		return err
	}
	if len(key) == 0 {
		if bytes.Equal(sanity, sanityText) {
			return nil
		}
		return ErrWrongPassword
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return err
	}
	plain := make([]byte, len(sanity))
	cipher.NewCTR(block, iv).XORKeyStream(plain, sanity)
	if !bytes.Equal(plain, sanityText) {
		return ErrWrongPassword
	}
	return nil
}

// LegacyEncryption returns the encryption of a legacy database: unencrypted
// (quick mode, whatever password was given) or SHA3-keyed.
func LegacyEncryption(kvdb string) (Encryption, Layout, error) {
	encrypted, err := ProbeLegacyEncryption(kvdb)
	if err != nil {
		return Encryption{}, Layout{}, err
	}
	if !encrypted {
		return Encryption{Scheme: SchemeNone}, Layout{
			BlobEncoding: blobcodec.LegacyRaw, ValueEncoding: EncodingRaw, BadgerCompression: EncodingSnappy,
		}, nil
	}
	return Encryption{Scheme: SchemeSHA3Legacy}, Layout{
		BlobEncoding: blobcodec.LegacySealed, ValueEncoding: EncodingZstd, BadgerCompression: EncodingZstd,
	}, nil
}

// Inference bounds: enough to be sure, never a full scan.
const (
	inferPrefixes      = 3
	inferKeysPerPrefix = 50_000
	inferChunkSamples  = 1000
	inferHashSamples   = 64
)

// InferLayout reconstructs the layout fields of a legacy database from its
// contents: chunk size and chunk hash from the relation keys of the most
// recent evidence, storage mode from block index files and a sample of chunk
// records. The encodings in base are kept. Fields it cannot
// infer are left zero and named in the returned Inference.
func InferLayout(db *badger.DB, root string, base Layout) (Layout, Inference) {
	l := base
	inf := Inference{At: time.Now().UTC()}
	decode := func(v []byte) ([]byte, error) {
		if l.ValueEncoding == EncodingRaw {
			return v, nil
		}
		return cnst.DECODER.DecodeAll(v, nil)
	}

	sizes, hashes := scanRelations(db, decode)
	switch len(sizes) {
	case 0:
		inf.Unverified = append(inf.Unverified, "chunk_kib")
	default:
		l.ChunkKiB = sizes[0]
		if len(sizes) > 1 {
			inf.Notes = append(inf.Notes, fmt.Sprintf("evidence uses different chunk sizes %v KiB; the most recent (%d KiB) applies", sizes, sizes[0]))
		}
	}
	switch {
	case hashes[64] > hashes[32]:
		l.ChunkHash = HashSHA3
	case hashes[32] > 0:
		l.ChunkHash = HashBLAKE3
	default:
		inf.Unverified = append(inf.Unverified, "chunk_hash")
	}
	if hashes[32] > 0 && hashes[64] > 0 {
		inf.Notes = append(inf.Notes, "both 32- and 64-byte chunk hashes are present")
	}

	l.Storage = inferStorage(db, root, decode)
	return l, inf
}

// scanRelations reads relation keys (R|||:<id>|||<offset>) newest first. It
// returns the chunk size of up to inferPrefixes objects, most recent first and
// without repeats, and counts the lengths of a sample of chunk hashes.
func scanRelations(db *badger.DB, decode func([]byte) ([]byte, error)) (sizesKiB []int, hashLens map[int]int) {
	hashLens = map[int]int{}
	ns := []byte(cnst.RelationNamespace)
	sep := []byte(cnst.DataSeperator)
	_ = db.View(func(txn *badger.Txn) error {
		opts := badger.DefaultIteratorOptions
		opts.PrefetchValues = false
		opts.Reverse = true
		opts.Prefix = ns
		it := txn.NewIterator(opts)
		defer it.Close()

		seen := map[int]bool{}
		var prefix []byte
		var gcd int64
		count, prefixes := 0, 0
		finish := func() {
			if gcd > 0 && gcd%1024 == 0 {
				if kib := int(gcd / 1024); !seen[kib] {
					seen[kib] = true
					sizesKiB = append(sizesKiB, kib)
				}
			}
			gcd, count = 0, 0
		}
		// Seek past the end of the namespace: reverse iteration starts at the
		// largest key not above the seek key.
		for it.Seek(append(append([]byte(nil), ns...), 0xff)); it.ValidForPrefix(ns); {
			key := it.Item().KeyCopy(nil)
			cut := bytes.LastIndex(key, sep)
			if cut < len(ns) {
				it.Next()
				continue
			}
			p, digits := key[len(ns):cut], key[cut+len(sep):]
			if !bytes.Equal(p, prefix) {
				finish()
				if prefixes == inferPrefixes {
					break
				}
				prefixes++
				prefix = append(prefix[:0], p...)
			}
			if off, err := strconv.ParseInt(string(digits), 10, 64); err == nil && off > 0 {
				gcd = gcdInt64(gcd, off)
			}
			if len(hashLens) == 0 || sumCounts(hashLens) < inferHashSamples {
				if v, err := it.Item().ValueCopy(nil); err == nil {
					if h, err := decode(v); err == nil {
						hashLens[len(h)]++
					}
				}
			}
			count++
			if count >= inferKeysPerPrefix {
				// Skip the rest of this object. Its keys all sort above
				// <ns><id><sep>, so a reverse seek there lands on the previous
				// object.
				it.Seek(append(append(append([]byte(nil), ns...), p...), sep...))
				continue
			}
			it.Next()
		}
		finish()
		return nil
	})
	return sizesKiB, hashLens
}

func gcdInt64(a, b int64) int64 {
	for b != 0 {
		a, b = b, a%b
	}
	return a
}

func sumCounts(m map[int]int) int {
	n := 0
	for _, v := range m {
		n += v
	}
	return n
}

// inferStorage reports container-bidx when block index files exist, container
// when sampled chunk records point into containers, and file otherwise.
func inferStorage(db *badger.DB, root string, decode func([]byte) ([]byte, error)) string {
	if bidx, _ := filepath.Glob(filepath.Join(root, cnst.BLOBSDIR, "blocks", "*.bidx")); len(bidx) > 0 {
		return StorageContainerBIDX
	}
	storage := StorageFile
	_ = db.View(func(txn *badger.Txn) error {
		opts := badger.DefaultIteratorOptions
		opts.Prefix = []byte(cnst.ChonkNamespace)
		it := txn.NewIterator(opts)
		defer it.Close()
		n := 0
		for it.Rewind(); it.Valid() && n < inferChunkSamples; it.Next() {
			n++
			v, err := it.Item().ValueCopy(nil)
			if err != nil {
				continue
			}
			if v, err = decode(v); err != nil {
				continue
			}
			var meta structs.ChonkMetadata
			if msgpackx.Unmarshal(v, &meta) == nil && meta.Container {
				storage = StorageContainer
				return nil
			}
		}
		return nil
	})
	return storage
}

// ErrNotDUES reports a directory that holds no DUES database.
var ErrNotDUES = errors.New("not a DUES database")

// HasDatabase reports whether root holds a DUES database: a manifest, or a
// legacy badger directory (MANIFEST and KEYREGISTRY).
func HasDatabase(root string) bool {
	if _, err := os.Stat(ManifestPath(root)); err == nil {
		return true
	}
	for _, name := range []string{"MANIFEST", keyRegistryName} {
		if _, err := os.Stat(filepath.Join(root, cnst.KVDBDIR, name)); err != nil {
			return false
		}
	}
	return true
}

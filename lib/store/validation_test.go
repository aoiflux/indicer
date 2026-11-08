package store

import (
	"bytes"
	"errors"
	"os"
	"strings"
	"testing"

	"indicer/internal/testutil"
	"indicer/internal/testutil/testdb"
	"indicer/lib/cnst"
	"indicer/lib/dbio"
	"indicer/lib/dbmeta"
	"indicer/lib/engine"
	"indicer/lib/fio"
	"indicer/lib/structs"
	"indicer/lib/util"

	"github.com/dgraph-io/badger/v4"
	"github.com/klauspost/compress/zstd"
	"github.com/vmihailenco/msgpack/v5"
)

// validationCase stores evidence and returns what ValidateEvidence needs,
// plus the key of its second chunk to damage.
func validationCase(t *testing.T, m testdb.Mode) (*testdb.DB, []byte, structs.EvidenceFile, []byte) {
	t.Helper()
	db := testdb.Open(t, m, testdb.Options{ChunkKiB: 4})
	cs := int(dbmeta.ChunkSize(db.DB))
	content := testutil.RandomBytes(31, 4*cs+9)
	id := runChunkPass(t, ingestChunksBatchOwner, db.DB, "evidence", content)
	chash, err := util.GetChonkHash(content[cs:2*cs], dbmeta.ChunkHasher(db.DB)())
	if err != nil {
		t.Fatal(err)
	}
	var ev structs.EvidenceFile
	ev.Start, ev.Size = 0, int64(len(content))
	return db, id, ev, util.AppendToBytesSlice(cnst.ChonkNamespace, chash)
}

// chunkFile returns the blob or container holding ckey, and the chunk's
// offset and size in it.
func chunkFile(t *testing.T, db *badger.DB, ckey []byte) (string, structs.ChonkMetadata) {
	t.Helper()
	loc, err := dbio.LocateChunk(ckey, db)
	if err != nil {
		t.Fatal(err)
	}
	return dbio.ResolveBlobPath(db.Opts().Dir, loc.Path), loc
}

func flipByte(t *testing.T, path string, off int64) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	b := []byte{0}
	if _, err := f.ReadAt(b, off); err != nil {
		t.Fatal(err)
	}
	b[0] ^= 0x40
	if _, err := f.WriteAt(b, off); err != nil {
		t.Fatal(err)
	}
}

// TestValidateEvidenceLevels checks what each validation level detects
// (invariant 7): a missing relation or chunk location at every level, a
// short blob or container from the material level, and damaged or
// misplaced bytes only from the content level.
func TestValidateEvidenceLevels(t *testing.T) {
	t.Parallel()
	type outcome struct{ locations, material, content error }
	cases := []struct {
		name   string
		modes  []testdb.Mode
		damage func(t *testing.T, db *badger.DB, fileID, ckey []byte)
		want   outcome
	}{
		{"intact", []testdb.Mode{testdb.Default, testdb.Quick, testdb.Container}, func(*testing.T, *badger.DB, []byte, []byte) {}, outcome{}},
		{"relation missing", []testdb.Mode{testdb.Default, testdb.Container}, func(t *testing.T, db *badger.DB, fileID, _ []byte) {
			key := util.AppendToBytesSlice(cnst.RelationNamespace, fileID, cnst.DataSeperator, dbmeta.ChunkSize(db))
			if err := db.Update(func(txn *badger.Txn) error { return txn.Delete(key) }); err != nil {
				t.Fatal(err)
			}
		}, outcome{ErrMaterialMissing, ErrMaterialMissing, ErrMaterialMissing}},
		{"chunk location missing", []testdb.Mode{testdb.Default, testdb.Container}, func(t *testing.T, db *badger.DB, _, ckey []byte) {
			if err := db.Update(func(txn *badger.Txn) error { return txn.Delete(ckey) }); err != nil {
				t.Fatal(err)
			}
		}, outcome{ErrMaterialMissing, ErrMaterialMissing, ErrMaterialMissing}},
		{"blob truncated", []testdb.Mode{testdb.Default, testdb.Quick}, func(t *testing.T, db *badger.DB, _, ckey []byte) {
			path, loc := chunkFile(t, db, ckey)
			if err := os.Truncate(path, loc.StoredSize-1); err != nil {
				t.Fatal(err)
			}
		}, outcome{nil, ErrMaterialMissing, ErrMaterialMissing}},
		{"blob deleted", []testdb.Mode{testdb.Default}, func(t *testing.T, db *badger.DB, _, ckey []byte) {
			path, _ := chunkFile(t, db, ckey)
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
		}, outcome{nil, ErrMaterialMissing, ErrMaterialMissing}},
		{"container truncated", []testdb.Mode{testdb.Container}, func(t *testing.T, db *badger.DB, _, ckey []byte) {
			path, loc := chunkFile(t, db, ckey)
			if err := os.Truncate(path, loc.Offset+loc.EncodedSize-1); err != nil {
				t.Fatal(err)
			}
		}, outcome{nil, ErrMaterialMissing, ErrMaterialMissing}},
		{"bit flipped", []testdb.Mode{testdb.Default, testdb.Quick, testdb.Container}, func(t *testing.T, db *badger.DB, _, ckey []byte) {
			path, loc := chunkFile(t, db, ckey)
			flipByte(t, path, loc.Offset+loc.EncodedSize/2)
		}, outcome{nil, nil, ErrMaterialCorrupt}},
		{"wrong container offset", []testdb.Mode{testdb.Container}, func(t *testing.T, db *badger.DB, _, ckey []byte) {
			_, loc := chunkFile(t, db, ckey)
			loc.Offset--
			raw, err := msgpack.Marshal(loc)
			if err != nil {
				t.Fatal(err)
			}
			if err := dbio.SetNode(ckey, raw, db); err != nil {
				t.Fatal(err)
			}
		}, outcome{nil, nil, ErrMaterialCorrupt}},
		// A legacy hierarchical database keeps chunk locations in block
		// index files; one record lost is a chunk with no location.
		{"block index record missing", []testdb.Mode{testdb.Container}, func(t *testing.T, db *badger.DB, _, ckey []byte) {
			testdb.MoveChunkMetaToBlockIndex(t, db)
			engine.For(db).SetBlockIndex(true)
			files, err := fio.BlockIndexFiles(dbio.KVDir(db))
			if err != nil || len(files) == 0 {
				t.Fatalf("block index files: %v, %v", files, err)
			}
			var keep []testutil.LegacyBlockRecord
			for _, f := range files {
				err := fio.ReadBlockFile(f, len(ckey)-len(cnst.ChonkNamespace), func(r fio.BlockIndexRecord) error {
					if !bytes.Equal(append([]byte(cnst.ChonkNamespace), r.Hash...), ckey) {
						keep = append(keep, testutil.LegacyBlockRecord{Hash: r.Hash, Path: r.Path, Offset: r.Offset, Size: r.Size})
					}
					return nil
				})
				if err != nil {
					t.Fatal(err)
				}
				if err := os.Remove(f); err != nil {
					t.Fatal(err)
				}
			}
			if err := testutil.WriteLegacyBlockIndex(util.BlobPath(dbio.KVDir(db)), keep); err != nil {
				t.Fatal(err)
			}
		}, outcome{ErrMaterialMissing, ErrMaterialMissing, ErrMaterialMissing}},
	}
	for _, c := range cases {
		for _, m := range c.modes {
			t.Run(c.name+"/"+m.Name, func(t *testing.T) {
				db, fileID, ev, ckey := validationCase(t, m)
				c.damage(t, db.DB, fileID, ckey)
				for level, want := range map[ValidationLevel]error{
					ValidateLocations: c.want.locations,
					ValidateMaterial:  c.want.material,
					ValidateContent:   c.want.content,
				} {
					err := ValidateEvidence(t.Context(), db.DB, fileID, ev, level)
					if want == nil && err != nil {
						t.Errorf("level %d: %v, want valid", level, err)
					}
					if want != nil && !errors.Is(err, want) {
						t.Errorf("level %d: %v, want %v", level, err, want)
					}
				}
			})
		}
	}
}

// TestValidateUsesEachEvidenceChunkSize: a legacy database can hold evidence
// stored at different chunk sizes. Validation must step through each one's
// relations at its own chunk size. At a smaller size it finds relations
// missing, so recovery would fail good evidence; at a larger one it skips
// chunks, so repair --verify would miss damage.
func TestValidateUsesEachEvidenceChunkSize(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t, testdb.Default, testdb.Options{ChunkKiB: 8})
	big := testutil.RandomBytes(5, 6*8192+3)
	bigID := runChunkPass(t, ingestChunksBatchOwner, db.DB, "evidence-at-8kib", big)
	db.Reconfigure(t, testdb.Options{ChunkKiB: 4})
	small := testutil.RandomBytes(6, 6*4096+3)
	smallID := runChunkPass(t, ingestChunksBatchOwner, db.DB, "evidence-at-4kib", small)

	var bigEv, smallEv structs.EvidenceFile
	bigEv.Size, smallEv.Size = int64(len(big)), int64(len(small))
	for _, level := range []ValidationLevel{ValidateLocations, ValidateMaterial, ValidateContent} {
		if err := ValidateEvidence(t.Context(), db.DB, bigID, bigEv, level); err != nil {
			t.Errorf("8 KiB evidence at 4 KiB, level %d: %v, want valid", level, err)
		}
	}

	// Damage the 4 KiB evidence's second chunk, which a validation stepping
	// at 8 KiB never reads.
	chash, err := util.GetChonkHash(small[4096:8192], dbmeta.ChunkHasher(db.DB)())
	if err != nil {
		t.Fatal(err)
	}
	path, loc := chunkFile(t, db.DB, util.AppendToBytesSlice(cnst.ChonkNamespace, chash))
	flipByte(t, path, loc.Offset+loc.EncodedSize/2)
	db.Reconfigure(t, testdb.Options{ChunkKiB: 8})
	if err := ValidateEvidence(t.Context(), db.DB, smallID, smallEv, ValidateContent); !errors.Is(err, ErrMaterialCorrupt) {
		t.Errorf("4 KiB evidence with a damaged chunk, at 8 KiB: %v, want %v", err, ErrMaterialCorrupt)
	}
}

// TestValidateCompressedLegacyContainer: legacy quick mode compressed a full
// container to <name>.blob.zst, while its chunks' metadata still names
// <name>.blob. Validation must find the compressed container, as reads do.
func TestValidateCompressedLegacyContainer(t *testing.T) {
	t.Parallel()
	db, id, ev, ckey := validationCase(t, testdb.Container)
	path, _ := chunkFile(t, db.DB, ckey)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	enc, err := zstd.NewWriter(nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(strings.TrimSuffix(path, cnst.BLOBEXT)+cnst.BLOBZSTEXT, enc.EncodeAll(raw, nil), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	for _, level := range []ValidationLevel{ValidateLocations, ValidateMaterial, ValidateContent} {
		if err := ValidateEvidence(t.Context(), db.DB, id, ev, level); err != nil {
			t.Errorf("level %d: %v, want valid", level, err)
		}
	}
}

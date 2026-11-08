package near

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/rand/v2"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"indicer/internal/progress"
	"indicer/internal/testutil/fsfixture"
	"indicer/internal/testutil/leakcheck"
	"indicer/internal/testutil/testdb"
	"indicer/lib/dbio"
	"indicer/lib/ingest"
	"indicer/lib/store"
	"indicer/lib/structs"
	"indicer/lib/util"

	"github.com/dgraph-io/badger/v4"
)

func TestMain(m *testing.M) {
	code := leakcheck.Run(m)
	fsfixture.Cleanup()
	os.Exit(code)
}

const cs = 4096 // chunk size of the test databases

func openNearDB(t *testing.T) *testdb.DB {
	t.Helper()
	d := testdb.Open(t, testdb.Default, testdb.Options{ChunkKiB: cs / 1024})
	if err := util.EnsureBlobPath(d.Root); err != nil {
		t.Fatal(err)
	}
	return d
}

func randomBytes(n int, seed uint64) []byte {
	r := rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(r.Uint32())
	}
	return b
}

// storeFile stores content as a file of its own, without an index, so its
// chunks belong to the evidence itself.
func storeFile(t *testing.T, db *badger.DB, name string, content []byte, revrel, simhash bool) (eid []byte, hash string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	res, err := ingest.Run(t.Context(), db, ingest.Request{Path: path, Decode: true, Quiet: true, NoIndex: true, RevRel: revrel, Simhash: simhash})
	if err != nil {
		t.Fatal(err)
	}
	e, err := dbio.GetEvidenceFile(res.EvidenceID, db)
	if err != nil {
		t.Fatal(err)
	}
	return res.EvidenceID, e.FileHash
}

func readNearReport(t *testing.T, path string) nearJSONReport {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var r nearJSONReport
	if err := json.Unmarshal(body, &r); err != nil {
		t.Fatal(err)
	}
	return r
}

func matchFor(r nearJSONReport, key []byte) (nearReportMatch, bool) {
	id := base64.StdEncoding.EncodeToString(key)
	for _, m := range r.Matches {
		if m.ID == id {
			return m, true
		}
	}
	return nearReportMatch{}, false
}

// TestNearNeedsReverseRelations (SRC-12): near on evidence stored without
// reverse relations is an error that says how to add them, not a bare "Key
// not found"; once added, the shared chunks are found, and the confidence is
// the matched share of the other evidence's chunks.
func TestNearNeedsReverseRelations(t *testing.T) {
	d := openNearDB(t)
	a := randomBytes(16*cs, 1)
	b := append(append([]byte(nil), a[:8*cs]...), randomBytes(8*cs, 2)...)
	ea, ha := storeFile(t, d.DB, "a.bin", a, false, false)
	eb, _ := storeFile(t, d.DB, "b.bin", b, false, false)

	out := filepath.Join(t.TempDir(), "near.json")
	if _, err := In(t.Context(), d.DB, ha, Options{Out: out}); !errors.Is(err, ErrNoReverseRelations) {
		t.Fatalf("near without reverse relations: %v, want ErrNoReverseRelations", err)
	}
	if _, err := os.Stat(out); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a refused near wrote a report: %v", err)
	}

	n, added, err := store.AddMissingReverseRelations(t.Context(), d.DB)
	if err != nil || n != 2 || added != 32 {
		t.Fatalf("add reverse relations: %d evidence, %d added, %v", n, added, err)
	}
	if n, _, err := store.AddMissingReverseRelations(t.Context(), d.DB); err != nil || n != 0 {
		t.Fatalf("a second add found %d evidence without them, %v", n, err)
	}

	o, err := In(t.Context(), d.DB, ha, Options{Out: out})
	if err != nil {
		t.Fatal(err)
	}
	r := readNearReport(t, out)
	if m, ok := matchFor(r, dbio.CanonicalEvidenceKey(ea)); !ok || !m.ExactMatch {
		t.Fatalf("the query is not its own exact match: %+v", m)
	}
	m, ok := matchFor(r, dbio.CanonicalEvidenceKey(eb))
	if !ok || m.ChunkMatchCount != 8 || m.ObjectChunks != 16 || m.Confidence != 0.5 {
		t.Fatalf("the half-shared evidence: %+v", m)
	}
	if o.Coverage.QueryChunks != 16 || o.Coverage.DistinctChunks != 16 || o.Coverage.MatchedChunks != 8 || len(r.Matches) != 2 {
		t.Fatalf("coverage %+v, %d matches", o.Coverage, len(r.Matches))
	}
}

// TestNearDurationCoversTheSearch: duration_ms is the whole search, from
// reading the query through phase 1, not only the report's own assembly.
func TestNearDurationCoversTheSearch(t *testing.T) {
	d := openNearDB(t)
	a := randomBytes(8*cs, 5)
	_, ha := storeFile(t, d.DB, "a.bin", a, true, false)
	path := filepath.Join(t.TempDir(), "a.bin")
	if err := os.WriteFile(path, a, 0o600); err != nil {
		t.Fatal(err)
	}
	var ctx context.Context
	for _, c := range []struct {
		name   string
		search func(out string) error
	}{
		{"in", func(out string) error { _, err := In(ctx, d.DB, ha, Options{Out: out}); return err }},
		{"out", func(out string) error {
			_, err := Out(ctx, d.DB, path, Options{Out: out, ExplainExact: true})
			return err
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			// Time passes only as the search works: each phase it starts or
			// ends moves the clock on by a second.
			var mu sync.Mutex
			clock := time.Date(2026, 11, 3, 9, 0, 0, 0, time.UTC)
			defer func(f func() time.Time) { now = f }(now)
			now = func() time.Time {
				mu.Lock()
				defer mu.Unlock()
				return clock
			}
			ctx = progress.With(t.Context(), progress.New(t.Context(), func() time.Time {
				mu.Lock()
				defer mu.Unlock()
				clock = clock.Add(time.Second)
				return clock
			}))
			out := filepath.Join(t.TempDir(), "near.json")
			if err := c.search(out); err != nil {
				t.Fatal(err)
			}
			// At least the query's reading, started and ended, and phase 1.
			if r := readNearReport(t, out); r.DurationMs < 3000 {
				t.Errorf("duration_ms = %d, want the whole search (3000 or more)", r.DurationMs)
			}
		})
	}
}

// TestNearConfidenceIsAShare (SRC-9): a chunk the query and a match both
// hold many times counts once per chunk of the match, so the confidence
// never passes 1.
func TestNearConfidenceIsAShare(t *testing.T) {
	d := openNearDB(t)
	x := randomBytes(cs, 3)
	a := bytes.Repeat(x, 4)
	b := append(bytes.Repeat(x, 3), randomBytes(cs, 4)...)
	_, ha := storeFile(t, d.DB, "a.bin", a, true, false)
	eb, _ := storeFile(t, d.DB, "b.bin", b, true, false)
	out := filepath.Join(t.TempDir(), "near.json")
	if _, err := In(t.Context(), d.DB, ha, Options{Out: out}); err != nil {
		t.Fatal(err)
	}
	m, ok := matchFor(readNearReport(t, out), dbio.CanonicalEvidenceKey(eb))
	if !ok || m.ChunkMatchCount != 3 || m.ObjectChunks != 4 || m.Confidence != 0.75 {
		t.Fatalf("match %+v", m)
	}
}

// TestNearAttributesToFiles: the chunks of one evidence found in another
// with the same filesystem are attributed to that evidence's files, and to
// its partition only where no file holds them; the query's own objects are
// not matches.
func TestNearAttributesToFiles(t *testing.T) {
	d := openNearDB(t)
	var img []byte
	for _, m := range fsfixture.All(t) {
		if m.Name == "fat/fat16" {
			var err error
			if img, err = os.ReadFile(fsfixture.Open(t, m)); err != nil {
				t.Fatal(err)
			}
		}
	}
	other := append([]byte(nil), img...)
	copy(other[len(other)-4:], "tail") // the last chunk differs, so it is another evidence
	put := func(name string, content []byte) []byte {
		path := filepath.Join(t.TempDir(), name)
		if err := os.WriteFile(path, content, 0o600); err != nil {
			t.Fatal(err)
		}
		res, err := ingest.Run(t.Context(), d.DB, ingest.Request{Path: path, Decode: true, Quiet: true, RevRel: true})
		if err != nil {
			t.Fatal(err)
		}
		return res.EvidenceID
	}
	ea, eb := put("a.img", img), put("b.img", other)
	e, err := dbio.GetEvidenceFile(ea, d.DB)
	if err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "near.json")
	if _, err := In(t.Context(), d.DB, e.FileHash, Options{Out: out}); err != nil {
		t.Fatal(err)
	}
	r := readNearReport(t, out)
	files := 0
	for _, m := range r.Matches {
		key, _ := base64.StdEncoding.DecodeString(m.ID)
		owner, err := ownerOf(d.DB, key)
		if err != nil {
			t.Fatal(err)
		}
		switch {
		case m.ExactMatch:
			if !bytes.Equal(owner, ea) {
				t.Errorf("exact match %s of another evidence", m.Names)
			}
		case !bytes.Equal(owner, eb):
			t.Errorf("match %s %s of the query's own evidence", m.Type, m.Names)
		case m.Type == "evidence":
			t.Errorf("the other evidence matched as a whole, not by its partition and files")
		case m.Type == "indexed":
			files++
			if m.Confidence <= 0 || m.Confidence > 1 {
				t.Errorf("%s: confidence %v", m.Names, m.Confidence)
			}
		}
	}
	if files == 0 {
		t.Fatalf("no file of the other evidence matched: %+v", r.Matches)
	}
}

// ownerOf is the evidence a record belongs to.
func ownerOf(db *badger.DB, key []byte) ([]byte, error) {
	switch nearFileType(key) {
	case "evidence":
		return dbio.EvidenceRawID(key), nil
	case "partition":
		rec, err := dbio.GetPartitionFile(key, db)
		if err != nil {
			return nil, err
		}
		return dbio.RecordEvidenceID(rec.IndexedFile)
	}
	rec, err := dbio.GetIndexedFile(key, db)
	if err != nil {
		return nil, err
	}
	return dbio.RecordEvidenceID(rec)
}

// TestNearOutReadsTheFile (SRC-4, SRC-11): near out reads the file in the
// database's chunks, without an empty last chunk when its size is a multiple
// of the chunk size, and closes it; a file stored whole is its exact match
// unless the chunks are asked for.
func TestNearOutReadsTheFile(t *testing.T) {
	d := openNearDB(t)
	a := randomBytes(8*cs, 5)
	ea, _ := storeFile(t, d.DB, "a.bin", a, true, false)
	path := filepath.Join(t.TempDir(), "query.bin")
	if err := os.WriteFile(path, a, 0o600); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "near.json")
	o, err := Out(t.Context(), d.DB, path, Options{Out: out})
	if err != nil {
		t.Fatal(err)
	}
	r := readNearReport(t, out)
	if m, ok := matchFor(r, dbio.CanonicalEvidenceKey(ea)); !ok || !m.ExactMatch || len(r.Matches) != 1 || o.Coverage.QueryChunks != 0 {
		t.Fatalf("a stored file: %+v, coverage %+v", r.Matches, o.Coverage)
	}
	o, err = Out(t.Context(), d.DB, path, Options{Out: out, Force: true, ExplainExact: true})
	if err != nil {
		t.Fatal(err)
	}
	if o.Coverage.QueryChunks != 8 || o.Coverage.MatchedChunks != 8 {
		t.Fatalf("chunks of an 8-chunk file: %+v", o.Coverage)
	}
	if m, ok := matchFor(readNearReport(t, out), dbio.CanonicalEvidenceKey(ea)); !ok || m.ChunkMatchCount != 8 {
		t.Fatalf("explained exact match %+v", m)
	}

	half := append(append([]byte(nil), a[:4*cs]...), randomBytes(4*cs+100, 6)...)
	if err := os.WriteFile(path, half, 0o600); err != nil {
		t.Fatal(err)
	}
	o, err = Out(t.Context(), d.DB, path, Options{Out: out, Force: true})
	if err != nil {
		t.Fatal(err)
	}
	m, ok := matchFor(readNearReport(t, out), dbio.CanonicalEvidenceKey(ea))
	if !ok || m.ExactMatch || m.ChunkMatchCount != 4 || m.Confidence != 0.5 || o.Coverage.QueryChunks != 9 {
		t.Fatalf("a half-stored file: %+v, coverage %+v", m, o.Coverage)
	}
	if err := os.Remove(path); err != nil {
		t.Fatalf("near out left the file open: %v", err)
	}
}

// TestNearReportRefusesExistingFile (SRC-11): a report never replaces a
// file unless asked to, and nothing is searched when it would.
func TestNearReportRefusesExistingFile(t *testing.T) {
	d := openNearDB(t)
	_, ha := storeFile(t, d.DB, "a.bin", randomBytes(4*cs, 7), true, false)
	out := filepath.Join(t.TempDir(), "near.json")
	if err := os.WriteFile(out, []byte("earlier work"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := In(t.Context(), d.DB, ha, Options{Out: out}); !errors.Is(err, ErrReportExists) {
		t.Fatalf("near in over an existing report: %v", err)
	}
	if _, err := Out(t.Context(), d.DB, out, Options{Out: out}); !errors.Is(err, ErrReportExists) {
		t.Fatalf("near out over an existing report: %v", err)
	}
	if body, _ := os.ReadFile(out); string(body) != "earlier work" {
		t.Fatalf("the existing file was changed: %q", body)
	}
	if _, err := In(t.Context(), d.DB, ha, Options{Out: out, Force: true}); err != nil {
		t.Fatal(err)
	}
}

// TestNearRunsAreIndependent (SRC-6): concurrent near searches report what
// each reports alone; before, every call reset package-wide result stores
// the others were writing.
func TestNearRunsAreIndependent(t *testing.T) {
	d := openNearDB(t)
	base := randomBytes(12*cs, 8)
	var hashes []string
	for i := range 4 {
		content := append(append([]byte(nil), base[:(i+2)*cs]...), randomBytes(4*cs, uint64(10+i))...)
		_, h := storeFile(t, d.DB, "f"+string(rune('a'+i))+".bin", content, true, false)
		hashes = append(hashes, h)
	}
	summarise := func(path string) map[string]float64 {
		out := map[string]float64{}
		for _, m := range readNearReport(t, path).Matches {
			out[m.ID] = m.Confidence
		}
		return out
	}
	dir := t.TempDir()
	alone := make([]map[string]float64, len(hashes))
	for i, h := range hashes {
		p := filepath.Join(dir, "alone"+string(rune('a'+i))+".json")
		if _, err := In(t.Context(), d.DB, h, Options{Out: p}); err != nil {
			t.Fatal(err)
		}
		alone[i] = summarise(p)
	}
	var wg sync.WaitGroup
	errs := make([]error, len(hashes)*3)
	for round := range 3 {
		for i, h := range hashes {
			wg.Go(func() {
				p := filepath.Join(dir, "together"+string(rune('a'+i))+string(rune('0'+round))+".json")
				_, errs[round*len(hashes)+i] = In(t.Context(), d.DB, h, Options{Out: p})
			})
		}
	}
	wg.Wait()
	for round := range 3 {
		for i := range hashes {
			if err := errs[round*len(hashes)+i]; err != nil {
				t.Fatal(err)
			}
			got := summarise(filepath.Join(dir, "together"+string(rune('a'+i))+string(rune('0'+round))+".json"))
			if len(got) != len(alone[i]) {
				t.Fatalf("query %d round %d: %d matches, alone %d", i, round, len(got), len(alone[i]))
			}
			for id, c := range alone[i] {
				if got[id] != c {
					t.Fatalf("query %d round %d: %s has confidence %v, alone %v", i, round, id, got[id], c)
				}
			}
		}
	}
}

// TestNearDeepMatchesSimilarChunks: deep mode matches chunks whose
// signatures are close, with their similarity as the confidence; without
// signatures it says so and reports exact matches only.
func TestNearDeepMatchesSimilarChunks(t *testing.T) {
	d := openNearDB(t)
	a := randomBytes(8*cs, 9)
	b := append([]byte(nil), a...)
	for i := 0; i < len(b); i += cs {
		b[i+100] ^= 0xff
		b[i+3000] ^= 0xff
	}
	_, ha := storeFile(t, d.DB, "a.bin", a, true, true)
	eb, _ := storeFile(t, d.DB, "b.bin", b, true, true)
	out := filepath.Join(t.TempDir(), "near.json")
	if _, err := In(t.Context(), d.DB, ha, Options{Out: out}); err != nil {
		t.Fatal(err)
	}
	if _, ok := matchFor(readNearReport(t, out), dbio.CanonicalEvidenceKey(eb)); ok {
		t.Fatal("chunks that differ matched without deep mode")
	}
	if _, err := In(t.Context(), d.DB, ha, Options{Out: out, Force: true, Deep: true}); err != nil {
		t.Fatal(err)
	}
	m, ok := matchFor(readNearReport(t, out), dbio.CanonicalEvidenceKey(eb))
	if !ok || m.DeepMatchCount != 8 || m.ChunkMatchCount != 8 || m.Confidence < minDeepConfidence || m.Confidence >= 1 {
		t.Fatalf("deep match %+v", m)
	}

	bare := openNearDB(t)
	_, hc := storeFile(t, bare.DB, "c.bin", a, true, false)
	storeFile(t, bare.DB, "d.bin", b, true, false)
	o, err := In(t.Context(), bare.DB, hc, Options{Out: out, Force: true, Deep: true})
	if err != nil {
		t.Fatal(err)
	}
	if o.Coverage.WithoutSignatures != 2 || o.Coverage.UnsignedQueryChunks != 8 || len(o.Notes) == 0 {
		t.Fatalf("deep mode without signatures: %+v, notes %q", o.Coverage, o.Notes)
	}
}

// TestNearSkipsCommonChunks (SRC-5): a chunk stored in more places than the
// cap is skipped and counted, not attributed occurrence by occurrence.
func TestNearSkipsCommonChunks(t *testing.T) {
	prev := maxOccurrences
	maxOccurrences = 3
	t.Cleanup(func() { maxOccurrences = prev })
	d := openNearDB(t)
	x := randomBytes(cs, 11)
	a := append(append([]byte(nil), x...), randomBytes(cs, 12)...)
	_, ha := storeFile(t, d.DB, "a.bin", a, true, false)
	eb, _ := storeFile(t, d.DB, "b.bin", bytes.Repeat(x, 5), true, false)
	out := filepath.Join(t.TempDir(), "near.json")
	o, err := In(t.Context(), d.DB, ha, Options{Out: out})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := matchFor(readNearReport(t, out), dbio.CanonicalEvidenceKey(eb)); ok || o.Coverage.CommonChunks != 1 || len(o.Notes) == 0 {
		t.Fatalf("a chunk in 6 places with a cap of 3: coverage %+v, notes %q", o.Coverage, o.Notes)
	}
}

func TestChunksOfExtents(t *testing.T) {
	ext := []structs.Extent{
		{Offset: 100, Length: 50},      // chunk 0
		{Offset: 4000, Length: 200},    // chunks 0 and 1
		{Offset: 3 * cs, Length: cs},   // chunk 3
		{Offset: 3*cs + 10, Length: 5}, // chunk 3 again
	}
	if n := chunksOfExtents(ext, cs); n != 3 {
		t.Fatalf("%d chunks, want 3", n)
	}
	if n := chunksOfRange(cs-1, cs+1, cs); n != 2 {
		t.Fatalf("%d chunks, want 2", n)
	}
}

// TestFileSimHashInPieces (SRC-4): the full-file SimHash does not depend on
// how the file is cut into pieces, now that a piece is no longer copied
// whole onto the carry.
func TestFileSimHashInPieces(t *testing.T) {
	data := randomBytes(10000, 13)
	whole := newFileSimHashAccumulator()
	whole.write(data)
	for _, cuts := range [][]int{{1}, {2, 3}, {3, 4, 5, 9000}, {4096, 8192}} {
		acc := newFileSimHashAccumulator()
		prev := 0
		for _, c := range cuts {
			acc.write(data[prev:c])
			prev = c
		}
		acc.write(data[prev:])
		if acc.finalize() != whole.finalize() {
			t.Fatalf("cut at %v: %x, whole %x", cuts, acc.finalize(), whole.finalize())
		}
	}
}

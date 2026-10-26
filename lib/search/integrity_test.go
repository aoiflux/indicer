package search

import (
	"bytes"
	"encoding/base64"
	"errors"
	"testing"
	"unicode/utf8"

	"indicer/internal/testutil/fsfixture"
	"indicer/lib/dbio"
	"indicer/lib/ingest"
	"indicer/lib/structs"

	"github.com/dgraph-io/badger/v4"
)

// TestSearchFindsEveryIndexedFile: in every fixture, each indexed file of
// at least 16 bytes is found by a search for 16 bytes of its own content
// (index-integrity invariant 1; list and restore are checked by
// internal/e2e). The window is taken where it is ASCII or not UTF-8, which
// match byte for byte.
func TestSearchFindsEveryIndexedFile(t *testing.T) {
	probed := 0
	for _, m := range fsfixture.All(t) {
		t.Run(m.Name, func(t *testing.T) {
			db := openSearchDB(t)
			res, err := ingest.Run(t.Context(), db, ingest.Request{Path: fsfixture.Open(t, m), Decode: true, Quiet: true})
			if err != nil {
				t.Fatal(err)
			}
			e, err := dbio.GetEvidenceFile(res.EvidenceID, db)
			if err != nil {
				t.Fatal(err)
			}
			type probe struct {
				key  string
				term string
			}
			var probes []probe
			for pidB64 := range e.InternalObjects {
				pid, _ := base64.StdEncoding.DecodeString(pidB64)
				prec, err := dbio.GetPartitionFile(pid, db)
				if err != nil {
					t.Fatal(err)
				}
				err = dbio.ForEachPartitionMember(db, pid, prec, func(iidB64 string, _ structs.InternalOffset) error {
					iid, _ := base64.StdEncoding.DecodeString(iidB64)
					key := dbio.CanonicalIndexedKey(iid)
					data, err := readAll(t, db, key)
					if errors.Is(err, dbio.ErrContentUnavailable) || len(data) < 16 {
						return nil
					}
					if err != nil {
						return err
					}
					if term, ok := window(data); ok {
						probes = append(probes, probe{string(key), term})
					}
					return nil
				})
				if err != nil {
					t.Fatal(err)
				}
			}
			probed += len(probes)
			for len(probes) > 0 {
				batch := probes[:min(len(probes), 16)]
				probes = probes[len(batch):]
				terms := make([]string, len(batch))
				for i, p := range batch {
					terms[i] = p.term
				}
				r, err := Run(t.Context(), db, terms, Options{})
				if err != nil {
					t.Fatal(err)
				}
				for i, p := range batch {
					found := false
					for _, h := range r.Hits {
						if string(h.Key) == p.key && h.Counts[i] > 0 {
							found = true
						}
					}
					if !found {
						t.Errorf("%q: a search for 16 bytes of its content does not find it", p.key)
					}
				}
			}
		})
	}
	if probed < 100 {
		t.Fatalf("only %d files were searched for", probed)
	}
}

func readAll(t *testing.T, db *badger.DB, key []byte) ([]byte, error) {
	t.Helper()
	l, err := dbio.OpenLogical(db, key, dbio.ReadOptions{})
	if err != nil {
		return nil, err
	}
	var data []byte
	err = l.Stream(t.Context(), func(b []byte) error { data = append(data, b...); return nil })
	return data, err
}

// window returns 16 bytes of data that match byte for byte: ASCII, or not
// valid UTF-8.
func window(data []byte) (string, bool) {
	for off := len(data) / 2; off+16 <= len(data); off += 7 {
		w := data[off : off+16]
		if isASCII(w) || !utf8.Valid(w) {
			return string(bytes.Clone(w)), true
		}
	}
	for off := 0; off+16 <= len(data)/2; off += 7 {
		w := data[off : off+16]
		if isASCII(w) || !utf8.Valid(w) {
			return string(bytes.Clone(w)), true
		}
	}
	return "", false
}

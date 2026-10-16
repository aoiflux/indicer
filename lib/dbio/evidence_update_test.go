package dbio

import (
	"errors"
	"fmt"
	"sync"
	"testing"

	"indicer/lib/cnst"
	"indicer/lib/structs"

	"github.com/dgraph-io/badger/v4"
)

// TestUpdateEvidenceConcurrentWritersKeepEveryChange runs many concurrent
// read-modify-writes of one evidence record, each adding its own partition.
// Badger rejects all but one of a set of overlapping transactions with
// ErrConflict; UpdateEvidence must retry those, so no change is lost.
func TestUpdateEvidenceConcurrentWritersKeepEveryChange(t *testing.T) {
	for _, quick := range []bool{true, false} {
		t.Run(fmt.Sprintf("quick=%v", quick), func(t *testing.T) {
			db := openDBIOTestDB(t)
			cnst.QUICKOPT = quick
			eid := []byte("evidence-concurrent")
			if err := CreateEvidence(db, eid, structs.NewEvidenceFile("e.dd", 0, 1, nil, "raw", "")); err != nil {
				t.Fatal(err)
			}

			const writers, perWriter = 8, 12
			var wg sync.WaitGroup
			errs := make(chan error, writers*perWriter)
			for w := range writers {
				wg.Add(1)
				go func() {
					defer wg.Done()
					for i := range perWriter {
						errs <- UpdateEvidence(db, eid, func(_ *badger.Txn, e *structs.EvidenceFile) error {
							if e.InternalObjects == nil {
								e.InternalObjects = map[string]structs.InternalOffset{}
							}
							e.InternalObjects[fmt.Sprintf("p-%d-%d", w, i)] = structs.InternalOffset{Start: int64(i)}
							return nil
						})
					}
				}()
			}
			wg.Wait()
			close(errs)
			for err := range errs {
				if err != nil {
					t.Fatalf("UpdateEvidence: %v", err)
				}
			}

			got, err := GetEvidenceFile(eid, db)
			if err != nil {
				t.Fatal(err)
			}
			if len(got.InternalObjects) != writers*perWriter {
				t.Fatalf("record has %d partitions, want %d: updates were lost", len(got.InternalObjects), writers*perWriter)
			}
		})
	}
}

func TestUpdateEvidenceOutcomes(t *testing.T) {
	db := openDBIOTestDB(t)
	eid := []byte("evidence-outcomes")

	missing := UpdateEvidence(db, eid, func(*badger.Txn, *structs.EvidenceFile) error { return nil })
	if !errors.Is(missing, badger.ErrKeyNotFound) {
		t.Fatalf("update of a missing record = %v, want ErrKeyNotFound", missing)
	}

	if err := CreateEvidence(db, eid, structs.NewEvidenceFile("first", 0, 1, nil, "raw", "")); err != nil {
		t.Fatal(err)
	}
	if err := CreateEvidence(db, eid, structs.NewEvidenceFile("second", 0, 1, nil, "raw", "")); !errors.Is(err, ErrEvidenceExists) {
		t.Fatalf("second create = %v, want ErrEvidenceExists", err)
	}

	// ErrNoChange and a failing callback both leave the record untouched.
	noChange := UpdateEvidence(db, eid, func(_ *badger.Txn, e *structs.EvidenceFile) error {
		e.Name = "discarded"
		return ErrNoChange
	})
	if noChange != nil {
		t.Fatalf("ErrNoChange update = %v, want nil", noChange)
	}
	boom := errors.New("boom")
	if err := UpdateEvidence(db, eid, func(_ *badger.Txn, e *structs.EvidenceFile) error {
		e.Name = "discarded"
		return boom
	}); !errors.Is(err, boom) {
		t.Fatalf("failing update = %v, want its error", err)
	}

	// The callback's transaction commits with the record.
	extra := []byte("extra-key")
	if err := UpdateEvidence(db, eid, func(txn *badger.Txn, e *structs.EvidenceFile) error {
		e.Completed = true
		return txn.Set(extra, []byte("v"))
	}); err != nil {
		t.Fatal(err)
	}
	got, err := GetEvidenceFile(eid, db)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "first" || !got.Completed {
		t.Fatalf("record = name %q completed %v, want first/true", got.Name, got.Completed)
	}
	if err := PingNode(extra, db); err != nil {
		t.Fatalf("key set in the callback's transaction: %v", err)
	}
}

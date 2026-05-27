package store

import (
	"indicer/lib/dbio"

	"github.com/dgraph-io/badger/v4"
)

// revRelAppendBuffer is exclusively owned by the single batchOwnerWriteLoop
// goroutine — no synchronisation is required.
type revRelAppendBuffer struct {
	members []dbio.ReverseRelationAppendMember
}

func newRevRelAppendBuffer(capacity int) *revRelAppendBuffer {
	if capacity < 0 {
		capacity = 0
	}
	return &revRelAppendBuffer{members: make([]dbio.ReverseRelationAppendMember, 0, capacity)}
}

// add appends chash and index to the buffer. Ownership of chash transfers to
// the buffer at this point; callers must not modify chash after calling add.
func (b *revRelAppendBuffer) add(chash []byte, index int64) {
	if b == nil {
		return
	}
	member := dbio.ReverseRelationAppendMember{Chash: chash, Index: index}
	b.members = append(b.members, member)
}

func (b *revRelAppendBuffer) flush(fhash []byte, db *badger.DB, batch *badger.WriteBatch) error {
	if b == nil {
		return nil
	}
	members := b.members
	b.members = nil
	return dbio.SetReverseRelationSegmentMembers(fhash, members, db, batch)
}

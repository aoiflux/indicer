package store

import (
	"indicer/lib/dbio"
	"sync"

	"github.com/dgraph-io/badger/v4"
)

type revRelAppendBuffer struct {
	mu      sync.Mutex
	members []dbio.ReverseRelationAppendMember
}

func newRevRelAppendBuffer(capacity int) *revRelAppendBuffer {
	if capacity < 0 {
		capacity = 0
	}
	return &revRelAppendBuffer{members: make([]dbio.ReverseRelationAppendMember, 0, capacity)}
}

func (b *revRelAppendBuffer) add(chash []byte, index int64) {
	if b == nil {
		return
	}

	chashCopy := append([]byte(nil), chash...)
	member := dbio.ReverseRelationAppendMember{Chash: chashCopy, Index: index}

	b.mu.Lock()
	b.members = append(b.members, member)
	b.mu.Unlock()
}

func (b *revRelAppendBuffer) flush(fhash []byte, batch *badger.WriteBatch) error {
	if b == nil {
		return nil
	}

	b.mu.Lock()
	members := b.members
	b.members = nil
	b.mu.Unlock()

	return dbio.SetReverseRelationAppendMembers(fhash, members, batch)
}

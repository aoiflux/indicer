package signer

import (
	"bytes"
	"crypto/ed25519"
	"encoding/binary"
	"errors"
	"fmt"
	"sync"

	"github.com/aoiflux/graphene/store"
)

// ActorRef names the actor and key a commit or package is signed with.
type ActorRef struct {
	Name    string
	ActorID uint64
	KeyID   uint64
}

// Provider is a resolved signing identity: an actor's key, which signs, and
// the keyring's verifier, which checks every key the database has held.
// graphene asks for exactly 64-byte signatures; a provider gives nothing else.
type Provider interface {
	// Actor is the actor and key that sign.
	Actor() ActorRef
	// Public is the signing key's public half.
	Public() ed25519.PublicKey
	// Signer signs graph commits and packages.
	Signer() store.Signer
	// Verifier checks signatures against the keyring.
	Verifier() store.Verifier
	// Close clears the private key. Signing afterwards fails.
	Close() error
}

type provider struct {
	key      *Key
	verifier *Verifier
}

func (p *provider) Actor() ActorRef           { return p.key.entry.Ref() }
func (p *provider) Public() ed25519.PublicKey { return bytes.Clone(p.key.entry.Public) }
func (p *provider) Signer() store.Signer      { return p.key }
func (p *provider) Verifier() store.Verifier  { return p.verifier }
func (p *provider) Close() error              { p.key.Close(); return nil }

// Key is an actor's private key in memory. It implements graphene's
// store.Signer.
type Key struct {
	entry Entry
	mu    sync.RWMutex
	priv  ed25519.PrivateKey
}

// errClosed is signing with a key that was closed.
var errClosed = errors.New("the actor key is closed")

// KeyID implements store.Signer.
func (k *Key) KeyID() uint64 { return k.entry.KeyID }

// Sign implements store.Signer.
func (k *Key) Sign(data []byte) ([]byte, error) {
	k.mu.RLock()
	defer k.mu.RUnlock()
	if k.priv == nil {
		return nil, errClosed
	}
	return ed25519.Sign(k.priv, data), nil
}

// Close clears the private key.
func (k *Key) Close() {
	k.mu.Lock()
	defer k.mu.Unlock()
	clear(k.priv)
	k.priv = nil
}

// selfTestDomain starts the message a resolved key signs to prove itself.
const selfTestDomain = "dues.signer.self-test.v1\x00"

// selfTest checks that k is the keyring entry's key and signs as graphene
// needs: one 64-byte signature that verifies under the entry's public key.
func (k *Key) selfTest() error {
	k.mu.RLock()
	pub, _ := k.priv.Public().(ed25519.PublicKey)
	k.mu.RUnlock()
	if !bytes.Equal(pub, k.entry.Public) {
		return fmt.Errorf("the key file does not hold key %s of the keyring", FormatID(k.entry.KeyID))
	}
	msg := binary.BigEndian.AppendUint64([]byte(selfTestDomain), k.entry.KeyID)
	sig, err := k.Sign(msg)
	if err != nil {
		return err
	}
	if len(sig) != SignatureSize {
		return fmt.Errorf("key %s signs %d bytes, want %d", FormatID(k.entry.KeyID), len(sig), SignatureSize)
	}
	if !ed25519.Verify(k.entry.Public, msg, sig) {
		return fmt.Errorf("key %s's test signature does not verify", FormatID(k.entry.KeyID))
	}
	return nil
}

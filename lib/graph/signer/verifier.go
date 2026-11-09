package signer

import (
	"crypto/ed25519"
	"fmt"

	"github.com/aoiflux/graphene/signing"
)

// SignatureSize is the size of every signature graphene accepts on a commit
// or an attestation, and of an Ed25519 signature.
const SignatureSize = ed25519.SignatureSize

// Verifier checks signatures against every key a keyring has held, revoked
// ones included: a key revoked today still signed what it signed before. It
// implements graphene's store.Verifier.
type Verifier struct {
	keys map[uint64]Entry
}

// Verifier returns a verifier over r's keys as they are now.
func (r *Keyring) Verifier() *Verifier {
	v := &Verifier{keys: make(map[uint64]Entry, len(r.entries))}
	for _, e := range r.entries {
		v.keys[e.KeyID] = e
	}
	return v
}

// Verify reports whether sig is keyID's signature over data. A key the
// keyring does not hold is an error wrapping signing.ErrUnknownKey, and a
// signature that does not verify one wrapping signing.ErrBadSignature, so a
// caller can tell "cannot check" from "forged".
func (v *Verifier) Verify(keyID uint64, data, sig []byte) error {
	e, ok := v.keys[keyID]
	if !ok {
		return fmt.Errorf("%w: %s", signing.ErrUnknownKey, FormatID(keyID))
	}
	if len(sig) != SignatureSize || !ed25519.Verify(e.Public, data, sig) {
		return fmt.Errorf("%w under key %s of actor %q", signing.ErrBadSignature, FormatID(keyID), e.Actor)
	}
	return nil
}

// Key returns the entry of the key with ID id.
func (v *Verifier) Key(id uint64) (Entry, bool) {
	e, ok := v.keys[id]
	return e, ok
}

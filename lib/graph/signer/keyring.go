package signer

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"
)

// Entry is one public key in the keyring.
type Entry struct {
	KeyID   uint64
	ActorID uint64
	// Actor is the actor's name, so a keyring read elsewhere names who
	// signed.
	Actor      string
	Public     ed25519.PublicKey
	Protection Protection
	Created    time.Time
	// Revoked is when the key was stopped from signing; zero while it may.
	// Its earlier signatures still verify.
	Revoked      time.Time
	RevokeReason string
}

// IsRevoked reports whether e may no longer sign.
func (e Entry) IsRevoked() bool { return !e.Revoked.IsZero() }

// Ref is the reference to the actor and key a commit signed with e carries.
func (e Entry) Ref() ActorRef { return ActorRef{Name: e.Actor, ActorID: e.ActorID, KeyID: e.KeyID} }

// Keyring is the public half of a keystore: every key any actor of the
// database has had, in the order they were added. Keys stay after a rotation
// or a revocation, so history keeps verifying.
type Keyring struct {
	entries []Entry
}

// maxKeys bounds a keyring, so a tampered file cannot make a reader allocate
// without limit.
const maxKeys = 4096

const keyringVersion = 1

type keyringFile struct {
	Version int         `json:"version"`
	Keys    []entryJSON `json:"keys"`
}

type entryJSON struct {
	KeyID        string     `json:"key_id"`
	ActorID      string     `json:"actor_id"`
	Actor        string     `json:"actor"`
	PublicKey    string     `json:"public_key"`
	Protection   string     `json:"protection"`
	Created      time.Time  `json:"created_at"`
	Revoked      *time.Time `json:"revoked_at,omitempty"`
	RevokeReason string     `json:"revoke_reason,omitempty"`
}

// ParseKeyring reads a keyring.json. It checks that every entry is
// consistent: each key ID is its public key's, each actor ID its name's, and
// no key appears twice.
func ParseKeyring(b []byte) (*Keyring, error) {
	var f keyringFile
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("keyring: %w", err)
	}
	if dec.More() {
		return nil, errors.New("keyring: data after the keyring")
	}
	if f.Version != keyringVersion {
		return nil, fmt.Errorf("keyring version %d is not one this build reads", f.Version)
	}
	if len(f.Keys) > maxKeys {
		return nil, fmt.Errorf("keyring holds %d keys, more than %d", len(f.Keys), maxKeys)
	}
	r := &Keyring{}
	for i, j := range f.Keys {
		e, err := j.entry()
		if err != nil {
			return nil, fmt.Errorf("keyring key %d: %w", i, err)
		}
		if err := r.add(e); err != nil {
			return nil, fmt.Errorf("keyring key %d: %w", i, err)
		}
	}
	return r, nil
}

func (j entryJSON) entry() (Entry, error) {
	keyID, err := ParseID(j.KeyID)
	if err != nil {
		return Entry{}, err
	}
	actorID, err := ParseID(j.ActorID)
	if err != nil {
		return Entry{}, err
	}
	name, err := NormalizeName(j.Actor)
	if err != nil {
		return Entry{}, err
	}
	if name != j.Actor {
		return Entry{}, fmt.Errorf("actor name %q is not in canonical form", j.Actor)
	}
	pub, err := base64.StdEncoding.Strict().DecodeString(j.PublicKey)
	if err != nil || len(pub) != ed25519.PublicKeySize {
		return Entry{}, fmt.Errorf("public key of %s is not a base64 Ed25519 key", j.KeyID)
	}
	prot, err := ParseProtection(j.Protection)
	if err != nil {
		return Entry{}, err
	}
	e := Entry{
		KeyID: keyID, ActorID: actorID, Actor: name, Public: pub, Protection: prot,
		Created: j.Created, RevokeReason: j.RevokeReason,
	}
	if j.Revoked != nil {
		if j.Revoked.IsZero() {
			return Entry{}, fmt.Errorf("key %s has a zero revocation time", j.KeyID)
		}
		e.Revoked = *j.Revoked
	} else if j.RevokeReason != "" {
		return Entry{}, fmt.Errorf("key %s has a revocation reason but no revocation", j.KeyID)
	}
	return e, nil
}

// Marshal writes r as keyring.json.
func (r *Keyring) Marshal() ([]byte, error) {
	f := keyringFile{Version: keyringVersion, Keys: make([]entryJSON, 0, len(r.entries))}
	for _, e := range r.entries {
		j := entryJSON{
			KeyID: FormatID(e.KeyID), ActorID: FormatID(e.ActorID), Actor: e.Actor,
			PublicKey: base64.StdEncoding.EncodeToString(e.Public), Protection: e.Protection.String(),
			Created: e.Created, RevokeReason: e.RevokeReason,
		}
		if e.IsRevoked() {
			t := e.Revoked
			j.Revoked = &t
		}
		f.Keys = append(f.Keys, j)
	}
	b, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

// add appends e after checking it against itself and the keys r holds.
func (r *Keyring) add(e Entry) error {
	if len(r.entries) >= maxKeys {
		return fmt.Errorf("the keyring already holds %d keys", maxKeys)
	}
	if got := KeyID(e.Public); got != e.KeyID {
		return fmt.Errorf("key %s: its public key has ID %s", FormatID(e.KeyID), FormatID(got))
	}
	if got := ActorID(e.Actor); got != e.ActorID {
		return fmt.Errorf("key %s: actor %q has ID %s, not %s", FormatID(e.KeyID), e.Actor, FormatID(got), FormatID(e.ActorID))
	}
	if _, ok := r.Key(e.KeyID); ok {
		return fmt.Errorf("key %s appears twice", FormatID(e.KeyID))
	}
	r.entries = append(r.entries, e)
	return nil
}

// Entries returns the keys in the order they were added.
func (r *Keyring) Entries() []Entry { return slices.Clone(r.entries) }

// Len is how many keys r holds.
func (r *Keyring) Len() int { return len(r.entries) }

// Key returns the key with ID id.
func (r *Keyring) Key(id uint64) (Entry, bool) {
	for _, e := range r.entries {
		if e.KeyID == id {
			return e, true
		}
	}
	return Entry{}, false
}

// Current returns the key actor signs with: its newest key, unless that is
// revoked. A rotation retires the keys before it, so revoking the newest
// never brings an older one back.
func (r *Keyring) Current(actorID uint64) (Entry, bool) {
	for _, e := range slices.Backward(r.entries) {
		if e.ActorID == actorID {
			return e, !e.IsRevoked()
		}
	}
	return Entry{}, false
}

// HasActor reports whether actor has any key, revoked or not.
func (r *Keyring) HasActor(actorID uint64) bool {
	return slices.ContainsFunc(r.entries, func(e Entry) bool { return e.ActorID == actorID })
}

// revoke marks key id revoked at t for reason.
func (r *Keyring) revoke(id uint64, t time.Time, reason string) error {
	for i := range r.entries {
		e := &r.entries[i]
		if e.KeyID != id {
			continue
		}
		if e.IsRevoked() {
			return fmt.Errorf("key %s was already revoked on %s", FormatID(id), e.Revoked.Format(time.RFC3339))
		}
		e.Revoked, e.RevokeReason = t, reason
		return nil
	}
	return fmt.Errorf("%w: %s", ErrNoKey, FormatID(id))
}

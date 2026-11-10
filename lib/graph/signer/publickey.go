package signer

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// publicKeyFormat names the file dues actor export-public writes: one
// actor's public key, to be kept outside the data root as a trust anchor.
const (
	publicKeyFormat  = "dues-public-key"
	publicKeyVersion = 1
	maxPublicKeyLen  = 4 << 10
)

type publicKeyJSON struct {
	Format    string    `json:"format"`
	Version   int       `json:"version"`
	Actor     string    `json:"actor"`
	ActorID   string    `json:"actor_id"`
	KeyID     string    `json:"key_id"`
	PublicKey []byte    `json:"public_key"`
	Created   time.Time `json:"created_at"`
}

// MarshalPublicKey writes e's public key in the form ParsePublicKey reads.
func MarshalPublicKey(e Entry) ([]byte, error) {
	b, err := json.MarshalIndent(publicKeyJSON{
		Format: publicKeyFormat, Version: publicKeyVersion,
		Actor: e.Actor, ActorID: FormatID(e.ActorID), KeyID: FormatID(e.KeyID),
		PublicKey: e.Public, Created: e.Created.UTC(),
	}, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

// ReadPublicKeyFile reads the public key file at path.
func ReadPublicKeyFile(path string) (Entry, error) {
	b, err := readLimited(path, maxPublicKeyLen)
	if err != nil {
		return Entry{}, err
	}
	e, err := ParsePublicKey(b)
	if err != nil {
		return Entry{}, fmt.Errorf("%s: %w", path, err)
	}
	return e, nil
}

// ParsePublicKey reads a public key MarshalPublicKey wrote. It refuses one
// whose IDs are not the ones its name and key derive.
func ParsePublicKey(b []byte) (Entry, error) {
	if len(b) > maxPublicKeyLen {
		return Entry{}, fmt.Errorf("a public key file is at most %d bytes", maxPublicKeyLen)
	}
	var j publicKeyJSON
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&j); err != nil {
		return Entry{}, fmt.Errorf("not a DUES public key: %w", err)
	}
	if dec.More() {
		return Entry{}, errors.New("not a DUES public key: data after the key")
	}
	if j.Format != publicKeyFormat || j.Version != publicKeyVersion {
		return Entry{}, fmt.Errorf("not a DUES public key: format %q version %d", j.Format, j.Version)
	}
	if len(j.PublicKey) != ed25519.PublicKeySize {
		return Entry{}, fmt.Errorf("the public key is %d bytes, want %d", len(j.PublicKey), ed25519.PublicKeySize)
	}
	name, err := NormalizeName(j.Actor)
	if err != nil {
		return Entry{}, err
	}
	actorID, err := ParseID(j.ActorID)
	if err != nil {
		return Entry{}, err
	}
	keyID, err := ParseID(j.KeyID)
	if err != nil {
		return Entry{}, err
	}
	pub := ed25519.PublicKey(j.PublicKey)
	switch {
	case name != j.Actor:
		return Entry{}, fmt.Errorf("actor name %q is not in canonical form", j.Actor)
	case actorID != ActorID(name):
		return Entry{}, fmt.Errorf("actor ID %s is not the ID of actor %q", j.ActorID, name)
	case keyID != KeyID(pub):
		return Entry{}, fmt.Errorf("key ID %s is not the ID of the public key", j.KeyID)
	}
	return Entry{KeyID: keyID, ActorID: actorID, Actor: name, Public: pub, Created: j.Created}, nil
}

// Package signer holds the keys that sign DUES graph commits and packages:
// a keystore in the data root (dues.keys), the key files that protect each
// actor's Ed25519 seed, the keyring of public keys, and the verifier built
// from it.
//
// An actor is a named principal: the reserved "database" actor every
// writable database gets, or an examiner created with dues actor create. Its
// ID is derived from its name, so offline workstations agree on it without a
// registry; actors.json catches the rare collision when a name is added.
//
// A signature proves only that the holder of a key signed the bytes. Whoever
// holds the data root holds the database actor's key, so a signature is as
// strong as custody of the root; only a public key kept outside the root is a
// trust anchor.
package signer

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// DatabaseActor is the reserved actor that signs a database's own commits and
// packages when no other actor is named.
const DatabaseActor = "database"

// maxNameRunes bounds an actor name.
const maxNameRunes = 64

const (
	actorDomain = "dues.actor.v1\x00"
	keyDomain   = "dues.key.v1\x00"
)

// NormalizeName returns the canonical form of an actor name: trimmed and
// lower-cased. It refuses an empty name, one longer than 64 characters, and
// one with a character that cannot be printed.
func NormalizeName(name string) (string, error) {
	// Before lower-casing, which turns invalid bytes into U+FFFD.
	if !utf8.ValidString(name) {
		return "", fmt.Errorf("actor name %q is not valid UTF-8", name)
	}
	n := canonical(name)
	switch {
	case n == "":
		return "", errors.New("an actor name cannot be empty")
	case utf8.RuneCountInString(n) > maxNameRunes:
		return "", fmt.Errorf("actor name %q is longer than %d characters", name, maxNameRunes)
	}
	for _, r := range n {
		if !unicode.IsPrint(r) {
			return "", fmt.Errorf("actor name %q has a character that cannot be printed", name)
		}
	}
	return n, nil
}

func canonical(name string) string { return strings.ToLower(strings.TrimSpace(name)) }

// ActorID is the ID of the actor called name: the first 8 bytes of
// SHA-256("dues.actor.v1\0" + the trimmed, lower-cased name), big-endian.
// graphene reserves 0 for an unattributed commit, so 0 becomes 1.
func ActorID(name string) uint64 {
	return derive(actorDomain, []byte(canonical(name)))
}

// KeyID is the ID of the Ed25519 public key pub: the first 8 bytes of
// SHA-256("dues.key.v1\0" + pub), big-endian, with 0 becoming 1 as graphene
// reserves it for no key.
func KeyID(pub ed25519.PublicKey) uint64 {
	return derive(keyDomain, pub)
}

func derive(domain string, b []byte) uint64 {
	h := sha256.New()
	h.Write([]byte(domain))
	h.Write(b)
	id := binary.BigEndian.Uint64(h.Sum(nil)[:8])
	if id == 0 {
		return 1
	}
	return id
}

// FormatID writes an actor or key ID as the 16 hex digits the keystore files
// and reports use.
func FormatID(id uint64) string { return fmt.Sprintf("%016x", id) }

// ParseID reads an ID FormatID wrote. It refuses anything but 16 hex digits,
// and 0, which no actor or key has.
func ParseID(s string) (uint64, error) {
	if len(s) != 16 {
		return 0, fmt.Errorf("ID %q is not 16 hex digits", s)
	}
	id, err := strconv.ParseUint(s, 16, 64)
	if err != nil || FormatID(id) != s {
		return 0, fmt.Errorf("ID %q is not 16 lower-case hex digits", s)
	}
	if id == 0 {
		return 0, errors.New("ID 0 names no actor or key")
	}
	return id, nil
}

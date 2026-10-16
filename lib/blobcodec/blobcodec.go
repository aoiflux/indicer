// Package blobcodec encodes chunk payloads for blob and container storage.
//
// A database's encoding is fixed when it is created and recorded in its
// manifest (lib/dbmeta). Three encodings exist:
//
//   - legacy-sealed: zstd, then AES-GCM with a random nonce prepended. Blobs
//     written before the nonce was randomised used a key-derived nonce; Decode
//     still accepts them. Nothing binds a payload to its chunk, so a payload
//     read from the wrong place decrypts without error.
//   - legacy-raw: the chunk bytes as they are (legacy quick mode).
//   - v2: a two-byte header (version, flags), zstd, and for encrypted
//     databases AES-GCM whose additional data is the header and the chunk key.
//     Decode checks the plaintext against the chunk hash, so a payload read at
//     the wrong offset or a damaged blob is an error, never wrong content.
//
// The codec for a database is registered when dbopen opens it. A database
// opened another way (tests, older call paths) gets the legacy codec its
// globals imply, which is what every caller did before codecs existed.
package blobcodec

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha3"
	"errors"
	"fmt"
	"sync"

	"indicer/lib/cnst"
	"indicer/lib/util"

	"github.com/dgraph-io/badger/v4"
	"github.com/zeebo/blake3"
)

// Encoding names, as recorded in the manifest.
const (
	LegacySealed = "legacy-sealed"
	LegacyRaw    = "legacy-raw"
	V2           = "v2"
)

// ErrCorrupt reports a stored chunk that does not decode to the chunk its key
// names: damaged, truncated, read from the wrong offset, or encrypted under a
// different key.
var ErrCorrupt = errors.New("stored chunk is corrupt")

// Codec turns chunk bytes into what is stored, and back. ckey is the chunk's
// database key (cnst.ChonkNamespace followed by the chunk hash).
type Codec interface {
	Name() string
	Encode(ckey, plain []byte) ([]byte, error)
	Decode(ckey, stored []byte) ([]byte, error)
}

// legacySealed is zstd + AES-GCM, as written before manifests existed.
type legacySealed struct{ key []byte }

func (legacySealed) Name() string { return LegacySealed }

func (c legacySealed) Encode(_, plain []byte) ([]byte, error) {
	return util.SealAES(c.key, cnst.ENCODER.EncodeAll(plain, make([]byte, 0, len(plain))))
}

func (c legacySealed) Decode(_, stored []byte) ([]byte, error) {
	compressed, err := util.UnsealAES(c.key, stored)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrCorrupt, err)
	}
	plain, err := cnst.DECODER.DecodeAll(compressed, nil)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrCorrupt, err)
	}
	return plain, nil
}

// legacyRaw stores chunks unencoded, as legacy quick mode did.
type legacyRaw struct{}

func (legacyRaw) Name() string                            { return LegacyRaw }
func (legacyRaw) Encode(_, plain []byte) ([]byte, error)  { return plain, nil }
func (legacyRaw) Decode(_, stored []byte) ([]byte, error) { return stored, nil }

const (
	v2Version   = 2
	flagZstd    = 1 << 0
	flagSealed  = 1 << 1
	knownFlags  = flagZstd | flagSealed
	headerLen   = 2
	gcmNonceLen = 12
)

// v2 is the current encoding. aead is nil for unencrypted databases.
type v2 struct{ aead cipher.AEAD }

// NewV2 returns the v2 codec. key is the blob key (32 bytes) of an encrypted
// database, or nil for an unencrypted one.
func NewV2(key []byte) (Codec, error) {
	if key == nil {
		return v2{}, nil
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return v2{aead: aead}, nil
}

// NewLegacySealed returns the legacy sealed codec for key.
func NewLegacySealed(key []byte) Codec { return legacySealed{key: append([]byte(nil), key...)} }

// NewLegacyRaw returns the legacy raw codec.
func NewLegacyRaw() Codec { return legacyRaw{} }

func (v2) Name() string { return V2 }

func aad(header, ckey []byte) []byte {
	return append(append(make([]byte, 0, len(header)+len(ckey)), header...), ckey...)
}

func (c v2) Encode(ckey, plain []byte) ([]byte, error) {
	flags := byte(flagZstd)
	if c.aead != nil {
		flags |= flagSealed
	}
	header := []byte{v2Version, flags}
	payload := cnst.ENCODER.EncodeAll(plain, make([]byte, 0, len(plain)))
	if c.aead == nil {
		return append(header, payload...), nil
	}
	out := make([]byte, headerLen+gcmNonceLen, headerLen+gcmNonceLen+len(payload)+c.aead.Overhead())
	copy(out, header)
	nonce := out[headerLen : headerLen+gcmNonceLen]
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return c.aead.Seal(out, nonce, payload, aad(header, ckey)), nil
}

func (c v2) Decode(ckey, stored []byte) ([]byte, error) {
	if len(stored) < headerLen || stored[0] != v2Version || stored[1]&^knownFlags != 0 {
		return nil, fmt.Errorf("%w: not a v2 chunk", ErrCorrupt)
	}
	header, flags := stored[:headerLen], stored[1]
	// A sealed database never accepts an unsealed chunk, and the reverse:
	// either would let a substituted blob through.
	if (flags&flagSealed != 0) != (c.aead != nil) {
		return nil, fmt.Errorf("%w: chunk encryption does not match the database", ErrCorrupt)
	}
	payload := stored[headerLen:]
	if c.aead != nil {
		if len(payload) < gcmNonceLen+c.aead.Overhead() {
			return nil, fmt.Errorf("%w: truncated", ErrCorrupt)
		}
		var err error
		payload, err = c.aead.Open(nil, payload[:gcmNonceLen], payload[gcmNonceLen:], aad(header, ckey))
		if err != nil {
			return nil, fmt.Errorf("%w: authentication failed", ErrCorrupt)
		}
	}
	plain := payload
	if flags&flagZstd != 0 {
		var err error
		if plain, err = cnst.DECODER.DecodeAll(payload, nil); err != nil {
			return nil, fmt.Errorf("%w: %v", ErrCorrupt, err)
		}
	}
	if err := Verify(ckey, plain); err != nil {
		return nil, err
	}
	return plain, nil
}

// Verify checks plain against the hash in ckey. The hash length names the
// algorithm: 32 bytes is BLAKE3 (or SHA3-256), 64 bytes SHA3-512. A key whose
// hash length is unknown is not checked.
func Verify(ckey, plain []byte) error {
	hash := bytes.TrimPrefix(ckey, []byte(cnst.ChonkNamespace))
	switch len(hash) {
	case 32:
		b := blake3.Sum256(plain)
		if bytes.Equal(b[:], hash) {
			return nil
		}
		s := sha3.Sum256(plain)
		if bytes.Equal(s[:], hash) {
			return nil
		}
	case 64:
		s := sha3.Sum512(plain)
		if bytes.Equal(s[:], hash) {
			return nil
		}
	default:
		return nil
	}
	return fmt.Errorf("%w: content does not match its chunk hash", ErrCorrupt)
}

var registry sync.Map // *badger.DB -> Codec

// Register sets the codec for db. dbopen calls it when it opens a database.
func Register(db *badger.DB, c Codec) { registry.Store(db, c) }

// Unregister forgets db's codec. dbopen calls it when the database closes.
func Unregister(db *badger.DB) { registry.Delete(db) }

// For returns db's codec. A database dbopen did not open gets the legacy codec
// the globals imply: raw in quick mode, otherwise sealed with badger's key.
func For(db *badger.DB) Codec {
	if c, ok := registry.Load(db); ok {
		return c.(Codec)
	}
	if cnst.QUICKOPT {
		return legacyRaw{}
	}
	return legacySealed{key: db.Opts().EncryptionKey}
}

package signer

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"

	"golang.org/x/crypto/argon2"
)

// A key file holds one actor key's 32-byte Ed25519 seed:
//
//	magic "DUESKEY1" (8) | version 1 (1) | protection (1)
//	| key ID (8) | actor ID (8)                          big-endian
//	| passphrase only: time (4) | memory KiB (4) | threads (1) | salt (16)
//	| passphrase, database: nonce (12) | AES-256-GCM(seed) (48)
//	| none: seed (32)
//
// Everything before the nonce is the GCM additional data, so a file
// relabelled onto another key or actor, or given other KDF parameters, fails
// to decrypt.
const (
	keyMagic   = "DUESKEY1"
	keyVersion = 1

	headerSize    = len(keyMagic) + 1 + 1 + 8 + 8
	kdfSize       = 4 + 4 + 1 + saltSize
	saltSize      = 16
	nonceSize     = 12
	seedSize      = 32
	sealedSize    = seedSize + 16
	maxKeyFileLen = headerSize + kdfSize + nonceSize + sealedSize
)

// Protection is how a key file protects its seed.
type Protection uint8

const (
	// ProtectPassphrase wraps the seed under a key Argon2id derives from the
	// actor's passphrase.
	ProtectPassphrase Protection = 1
	// ProtectDatabase wraps the seed under the database's actor key, which
	// the database password derives (dbmeta Keys.Actor).
	ProtectDatabase Protection = 2
	// ProtectNone stores the seed as it is: the database actor of an
	// unencrypted database, whose file mode is all that keeps it.
	ProtectNone Protection = 3
)

func (p Protection) String() string {
	switch p {
	case ProtectPassphrase:
		return "passphrase"
	case ProtectDatabase:
		return "database"
	case ProtectNone:
		return "none"
	}
	return fmt.Sprintf("protection(%d)", uint8(p))
}

func ParseProtection(s string) (Protection, error) {
	for _, p := range []Protection{ProtectPassphrase, ProtectDatabase, ProtectNone} {
		if p.String() == s {
			return p, nil
		}
	}
	return 0, fmt.Errorf("unknown key protection %q", s)
}

// KDF is the Argon2id cost a passphrase-protected key file was sealed with.
type KDF struct {
	Time      uint32
	MemoryKiB uint32
	Threads   uint8
}

// DefaultKDF is what new passphrase-protected keys use: three passes over
// 64 MiB on four threads, as an encrypted database's password does.
var DefaultKDF = KDF{Time: 3, MemoryKiB: 64 << 10, Threads: 4}

// The KDF costs a key file may ask for. The upper bounds keep a tampered file
// from making an open run for hours or allocate past any machine.
const (
	maxKDFTime    = 16
	minKDFMemory  = 8 << 10
	maxKDFMemory  = 4 << 20
	maxKDFThreads = 64
)

func (k KDF) check() error {
	if k.Time < 1 || k.Time > maxKDFTime || k.MemoryKiB < minKDFMemory || k.MemoryKiB > maxKDFMemory ||
		k.Threads < 1 || k.Threads > maxKDFThreads {
		return fmt.Errorf("key file asks for Argon2id time %d, memory %d KiB, threads %d: outside what DUES writes", k.Time, k.MemoryKiB, k.Threads)
	}
	return nil
}

// Unlock is what opens a key file's seed: a passphrase, the database's actor
// key, or nothing for an unprotected key.
type Unlock struct {
	protection Protection
	secret     []byte
	kdf        KDF
}

// Passphrase unlocks, and seals with DefaultKDF, a key protected by pw.
func Passphrase(pw []byte) Unlock {
	return Unlock{protection: ProtectPassphrase, secret: pw, kdf: DefaultKDF}
}

// ForDatabase unlocks the database actor's key with actorKey, the
// database's Keys.Actor. An unencrypted database has none, and its key is
// unprotected.
func ForDatabase(actorKey []byte) Unlock {
	if len(actorKey) == 0 {
		return Unlock{protection: ProtectNone}
	}
	return Unlock{protection: ProtectDatabase, secret: actorKey}
}

// withKDF is u sealing with kdf; tests use a cheap one.
func (u Unlock) withKDF(kdf KDF) Unlock {
	u.kdf = kdf
	return u
}

// Protection is how a key sealed with u protects its seed.
func (u Unlock) Protection() Protection { return u.protection }

// ErrWrongPassphrase is the error opening a key file with the wrong
// passphrase or database key, or a file whose header was changed.
var ErrWrongPassphrase = errors.New("the passphrase does not open this actor key")

// ProtectionError is the error opening a key file with an unlock of another
// kind: a passphrase for a key the database protects, say.
type ProtectionError struct {
	Have, Want Protection
}

func (e *ProtectionError) Error() string {
	return fmt.Sprintf("the key is protected by %s, not %s", describe(e.Have), describe(e.Want))
}

// keyFile is a parsed key file.
type keyFile struct {
	protection Protection
	keyID      uint64
	actorID    uint64
	kdf        KDF
	salt       []byte
	nonce      []byte
	// body is the sealed seed, or the seed when protection is none.
	body []byte
	// aad is the bytes before the nonce.
	aad []byte
}

// parseKeyFile reads a key file. It checks the layout only: whether the seed
// opens is for open.
func parseKeyFile(b []byte) (keyFile, error) {
	if len(b) < headerSize || string(b[:len(keyMagic)]) != keyMagic {
		return keyFile{}, errors.New("not a DUES key file")
	}
	if v := b[len(keyMagic)]; v != keyVersion {
		return keyFile{}, fmt.Errorf("key file version %d is not one this build reads", v)
	}
	f := keyFile{
		protection: Protection(b[len(keyMagic)+1]),
		keyID:      binary.BigEndian.Uint64(b[len(keyMagic)+2:]),
		actorID:    binary.BigEndian.Uint64(b[len(keyMagic)+10:]),
	}
	if f.keyID == 0 || f.actorID == 0 {
		return keyFile{}, errors.New("key file names key or actor 0")
	}
	rest := b[headerSize:]
	var want int
	switch f.protection {
	case ProtectPassphrase:
		want = kdfSize + nonceSize + sealedSize
	case ProtectDatabase:
		want = nonceSize + sealedSize
	case ProtectNone:
		want = seedSize
	default:
		return keyFile{}, fmt.Errorf("key file has unknown protection %d", uint8(f.protection))
	}
	if len(rest) != want {
		return keyFile{}, fmt.Errorf("%s key file is %d bytes, want %d", f.protection, len(b), headerSize+want)
	}
	if f.protection == ProtectPassphrase {
		f.kdf = KDF{
			Time:      binary.BigEndian.Uint32(rest),
			MemoryKiB: binary.BigEndian.Uint32(rest[4:]),
			Threads:   rest[8],
		}
		if err := f.kdf.check(); err != nil {
			return keyFile{}, err
		}
		f.salt = rest[9:kdfSize]
		rest = rest[kdfSize:]
	}
	if f.protection != ProtectNone {
		f.nonce, rest = rest[:nonceSize], rest[nonceSize:]
	}
	f.body = rest
	f.aad = b[:len(b)-len(rest)-len(f.nonce)]
	return f, nil
}

// sealKey writes the key file for seed, the seed of key keyID of actor
// actorID, protected as u says.
func sealKey(seed []byte, keyID, actorID uint64, u Unlock) ([]byte, error) {
	if len(seed) != seedSize {
		return nil, fmt.Errorf("seed is %d bytes, want %d", len(seed), seedSize)
	}
	b := make([]byte, 0, maxKeyFileLen)
	b = append(b, keyMagic...)
	b = append(b, keyVersion, byte(u.protection))
	b = binary.BigEndian.AppendUint64(b, keyID)
	b = binary.BigEndian.AppendUint64(b, actorID)
	var aesKey []byte
	switch u.protection {
	case ProtectNone:
		return append(b, seed...), nil
	case ProtectPassphrase:
		if err := u.kdf.check(); err != nil {
			return nil, err
		}
		salt := make([]byte, saltSize)
		if _, err := rand.Read(salt); err != nil {
			return nil, err
		}
		b = binary.BigEndian.AppendUint32(b, u.kdf.Time)
		b = binary.BigEndian.AppendUint32(b, u.kdf.MemoryKiB)
		b = append(b, u.kdf.Threads)
		b = append(b, salt...)
		aesKey = argon2.IDKey(u.secret, salt, u.kdf.Time, u.kdf.MemoryKiB, u.kdf.Threads, 32)
		defer clear(aesKey)
	case ProtectDatabase:
		aesKey = u.secret
	default:
		return nil, errors.New("no protection given for the key")
	}
	aead, err := newGCM(aesKey)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, nonceSize)
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	aad := bytes.Clone(b)
	b = append(b, nonce...)
	return aead.Seal(b, nonce, seed, aad), nil
}

// open returns the seed, which the caller clears when done.
func (f keyFile) open(u Unlock) ([]byte, error) {
	if u.protection != f.protection {
		return nil, &ProtectionError{Have: f.protection, Want: u.protection}
	}
	var aesKey []byte
	switch f.protection {
	case ProtectNone:
		return bytes.Clone(f.body), nil
	case ProtectPassphrase:
		aesKey = argon2.IDKey(u.secret, f.salt, f.kdf.Time, f.kdf.MemoryKiB, f.kdf.Threads, 32)
		defer clear(aesKey)
	case ProtectDatabase:
		aesKey = u.secret
	}
	aead, err := newGCM(aesKey)
	if err != nil {
		return nil, err
	}
	seed, err := aead.Open(nil, f.nonce, f.body, f.aad)
	if err != nil {
		return nil, ErrWrongPassphrase
	}
	return seed, nil
}

func describe(p Protection) string {
	switch p {
	case ProtectPassphrase:
		return "a passphrase"
	case ProtectDatabase:
		return "the database password"
	case ProtectNone:
		return "nothing"
	}
	return p.String()
}

func newGCM(key []byte) (cipher.AEAD, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("key wrapping key is %d bytes, want 32", len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

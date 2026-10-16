package dbmeta

import (
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/sha3"
	"crypto/subtle"
	"errors"

	"golang.org/x/crypto/argon2"
)

// KDFParams are the Argon2id parameters of a database. Salt is per database.
type KDFParams struct {
	Salt      []byte `json:"salt"`
	Time      uint32 `json:"time"`
	MemoryKiB uint32 `json:"memory_kib"`
	Threads   uint8  `json:"threads"`
}

// DefaultKDF is what new encrypted databases use: Argon2id with three passes
// over 64 MiB on four threads.
var DefaultKDF = KDFParams{Time: 3, MemoryKiB: 64 << 10, Threads: 4}

// Keys are the secrets an open database uses. Both are nil when it is not
// encrypted.
type Keys struct {
	// Badger is badger's encryption key.
	Badger []byte
	// Blob encrypts chunk blobs and containers.
	Blob []byte
}

// Credentials carry what the user supplied to unlock a database.
type Credentials struct {
	password []byte
	set      bool
	rawKey   []byte
}

// NoPassword is the credentials of a user who gave no password.
func NoPassword() Credentials { return Credentials{} }

// Password returns credentials for pw. An empty password is a password: it
// is what a legacy database written without --password was keyed with.
func Password(pw string) Credentials {
	return Credentials{password: []byte(pw), set: true}
}

// RawKey returns credentials holding a legacy key directly, as tests and
// callers that still pass a key do. It unlocks only legacy (sha3) databases.
func RawKey(key []byte) Credentials {
	return Credentials{rawKey: append([]byte(nil), key...), set: true}
}

// IsSet reports whether a password or key was supplied.
func (c Credentials) IsSet() bool { return c.set }

// Equal reports whether c and o hold the same password or key.
func (c Credentials) Equal(o Credentials) bool {
	return c.set == o.set &&
		subtle.ConstantTimeCompare(c.password, o.password) == 1 &&
		subtle.ConstantTimeCompare(c.rawKey, o.rawKey) == 1
}

// legacyKey is the key a legacy database derived from the password: an
// unsalted SHA3-256 (CFG-3). No password meant the hash of "".
func (c Credentials) legacyKey() []byte {
	if c.rawKey != nil {
		return c.rawKey
	}
	k := sha3.Sum256(c.password)
	return k[:]
}

// Errors from DeriveKeys.
var (
	ErrPasswordRequired = errors.New("database is encrypted; a password is required")
	ErrWrongPassword    = errors.New("wrong password for this database")
)

const (
	infoBadger = "dues/badger/v1"
	infoBlob   = "dues/blob/v1"
	infoCheck  = "dues/key-check/v1"
	checkText  = "DUES key check v1"
)

// NewEncryption sets up encryption for a new database. Without a password the
// database is unencrypted (D8); with one it is keyed with Argon2id.
func NewEncryption(c Credentials, params KDFParams) (Encryption, Keys, error) {
	if !c.set {
		return Encryption{Scheme: SchemeNone}, Keys{}, nil
	}
	if c.rawKey != nil {
		return Encryption{}, Keys{}, errors.New("a raw key cannot create a database; give a password")
	}
	params.Salt = make([]byte, 16)
	if _, err := rand.Read(params.Salt); err != nil {
		return Encryption{}, Keys{}, err
	}
	keys, check, err := argonKeys(c.password, params)
	if err != nil {
		return Encryption{}, Keys{}, err
	}
	return Encryption{Scheme: SchemeArgon2id, KDF: &params, Check: check}, keys, nil
}

func argonKeys(password []byte, p KDFParams) (Keys, []byte, error) {
	master := argon2.IDKey(password, p.Salt, p.Time, p.MemoryKiB, p.Threads, 32)
	derive := func(info string) ([]byte, error) {
		return hkdf.Key(sha256.New, master, p.Salt, info, 32)
	}
	badger, err := derive(infoBadger)
	if err != nil {
		return Keys{}, nil, err
	}
	blob, err := derive(infoBlob)
	if err != nil {
		return Keys{}, nil, err
	}
	checkKey, err := derive(infoCheck)
	if err != nil {
		return Keys{}, nil, err
	}
	mac := hmac.New(sha256.New, checkKey)
	mac.Write([]byte(checkText))
	return Keys{Badger: badger, Blob: blob}, mac.Sum(nil), nil
}

// DeriveKeys returns the keys for a database encrypted as e. It checks an
// Argon2id password against e.Check, so a wrong password is reported before
// badger is opened. For legacy databases the caller checks the key against
// badger's key registry (CheckLegacyKey).
func DeriveKeys(e Encryption, c Credentials) (Keys, error) {
	switch e.Scheme {
	case SchemeNone:
		return Keys{}, nil
	case SchemeSHA3Legacy:
		// No password meant the hash of the empty password.
		k := c.legacyKey()
		return Keys{Badger: k, Blob: k}, nil
	case SchemeArgon2id:
		if !c.set {
			return Keys{}, ErrPasswordRequired
		}
		if c.rawKey != nil {
			return Keys{}, errors.New("a raw key cannot unlock an argon2id database")
		}
		if e.KDF == nil {
			return Keys{}, errors.New("manifest has no KDF parameters")
		}
		keys, check, err := argonKeys(c.password, *e.KDF)
		if err != nil {
			return Keys{}, err
		}
		if !hmac.Equal(check, e.Check) {
			return Keys{}, ErrWrongPassword
		}
		return keys, nil
	}
	return Keys{}, errors.New("unknown encryption scheme " + e.Scheme)
}

// Package dbmeta describes a DUES database: its manifest (the layout and
// encryption every reader and writer must agree on), the keys derived from a
// password, and what can be inferred about a database written before
// manifests existed.
//
// The manifest is stored three times: <root>/dues-manifest.json (plaintext,
// no secrets), a mirror in the badger directory, and inside badger under
// MarkerKey. The two files let DUES learn the layout before opening badger;
// the marker, encrypted with the database when it is encrypted, is what a
// file is checked against.
package dbmeta

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"indicer/lib/blobcodec"
	"indicer/lib/cnst"
	"indicer/lib/fsx"
)

// Manifest identification.
const (
	FormatName    = "dues-db"
	FormatVersion = 1

	// ManifestName is the manifest file in the data root.
	ManifestName = "dues-manifest.json"
	// MirrorName is the manifest's copy in the badger directory.
	MirrorName = "DUES-MANIFEST.mirror.json"
	// MarkerKey holds the manifest inside badger.
	MarkerKey = "M|||:format"
)

// Layout values.
const (
	HashBLAKE3 = cnst.BLAKE3
	HashSHA3   = cnst.SHA3

	StorageFile          = "file"
	StorageContainer     = "container"
	StorageContainerBIDX = "container-bidx"

	EncodingZstd   = "zstd"
	EncodingSnappy = "snappy"
	EncodingRaw    = "raw"
	EncodingNone   = "none"
)

// Encryption schemes.
const (
	SchemeNone       = "none"
	SchemeSHA3Legacy = "sha3-legacy"
	SchemeArgon2id   = "argon2id"
)

// Manifest is what a database promises about itself.
type Manifest struct {
	Format     string     `json:"format"`
	Version    int        `json:"version"`
	Created    time.Time  `json:"created"`
	WrittenBy  string     `json:"written_by,omitempty"`
	Layout     Layout     `json:"layout"`
	Encryption Encryption `json:"encryption"`
	// Inferred is set when the manifest was reconstructed from a database
	// written before manifests existed.
	Inferred *Inference `json:"inferred,omitempty"`
}

// Layout is how data is laid out. None of it may change once data is written.
type Layout struct {
	ChunkKiB  int    `json:"chunk_kib"`
	ChunkHash string `json:"chunk_hash"`
	Storage   string `json:"storage"`
	// BlobEncoding is a lib/blobcodec encoding name.
	BlobEncoding string `json:"blob_encoding"`
	// ValueEncoding is how badger values are encoded: zstd, or raw for legacy
	// quick-mode databases.
	ValueEncoding string `json:"value_encoding"`
	// BadgerCompression is badger's table compression: zstd, snappy (legacy
	// quick mode, badger's default) or none.
	BadgerCompression string `json:"badger_compression"`
}

// Encryption is how the database is keyed. It holds no secret: Check is an
// HMAC that tells a right password from a wrong one.
type Encryption struct {
	Scheme string     `json:"scheme"`
	KDF    *KDFParams `json:"kdf,omitempty"`
	Check  []byte     `json:"check,omitempty"`
}

// Inference records how a legacy database's manifest was reconstructed.
type Inference struct {
	At time.Time `json:"at"`
	// Unverified names the layout fields that could not be inferred and were
	// taken from the command line or defaults.
	Unverified []string `json:"unverified,omitempty"`
	Notes      []string `json:"notes,omitempty"`
}

// Encrypted reports whether the database's data is encrypted.
func (m Manifest) Encrypted() bool { return m.Encryption.Scheme != SchemeNone }

// ErrNewerFormat reports a database written by a newer DUES.
var ErrNewerFormat = errors.New("database was written by a newer version of DUES")

// Validate checks that m is internally consistent and supported.
func (m Manifest) Validate() error {
	if m.Format != FormatName {
		return fmt.Errorf("manifest: format %q, want %q", m.Format, FormatName)
	}
	if m.Version > FormatVersion {
		return fmt.Errorf("%w (format version %d; this DUES reads up to %d)", ErrNewerFormat, m.Version, FormatVersion)
	}
	if m.Version < 1 {
		return fmt.Errorf("manifest: invalid version %d", m.Version)
	}
	l := m.Layout
	if l.ChunkKiB < 4 || l.ChunkKiB > 65536 || l.ChunkKiB%4 != 0 {
		return fmt.Errorf("manifest: invalid chunk size %d KiB", l.ChunkKiB)
	}
	if l.ChunkHash != HashBLAKE3 && l.ChunkHash != HashSHA3 {
		return fmt.Errorf("manifest: unknown chunk hash %q", l.ChunkHash)
	}
	switch l.Storage {
	case StorageFile, StorageContainer, StorageContainerBIDX:
	default:
		return fmt.Errorf("manifest: unknown storage %q", l.Storage)
	}
	if l.ValueEncoding != EncodingZstd && l.ValueEncoding != EncodingRaw {
		return fmt.Errorf("manifest: unknown value encoding %q", l.ValueEncoding)
	}
	switch l.BadgerCompression {
	case EncodingZstd, EncodingSnappy, EncodingNone:
	default:
		return fmt.Errorf("manifest: unknown badger compression %q", l.BadgerCompression)
	}

	// The encodings that go with each scheme. Anything else would read data
	// with the wrong codec or store plaintext in an encrypted database.
	e := m.Encryption
	switch e.Scheme {
	case SchemeNone:
		if l.BlobEncoding != blobcodec.V2 && l.BlobEncoding != blobcodec.LegacyRaw {
			return fmt.Errorf("manifest: blob encoding %q needs a key", l.BlobEncoding)
		}
	case SchemeSHA3Legacy:
		if l.BlobEncoding != blobcodec.LegacySealed {
			return fmt.Errorf("manifest: %s databases use %s blobs, not %q", e.Scheme, blobcodec.LegacySealed, l.BlobEncoding)
		}
	case SchemeArgon2id:
		if l.BlobEncoding != blobcodec.V2 {
			return fmt.Errorf("manifest: %s databases use %s blobs, not %q", e.Scheme, blobcodec.V2, l.BlobEncoding)
		}
		if e.KDF == nil || len(e.KDF.Salt) < 16 || e.KDF.Time == 0 || e.KDF.MemoryKiB < 8 || e.KDF.Threads == 0 || len(e.Check) != 32 {
			return errors.New("manifest: incomplete argon2id parameters")
		}
	default:
		return fmt.Errorf("manifest: unknown encryption scheme %q", e.Scheme)
	}
	if (l.ValueEncoding == EncodingRaw) != (l.BlobEncoding == blobcodec.LegacyRaw) {
		return errors.New("manifest: raw values and raw blobs go together (legacy quick mode)")
	}
	return nil
}

// SameLayout reports whether a and b describe the same data: equal layout
// and encryption. Creation time, writer and inference notes may differ.
// Container and container-bidx storage count as the same: they store chunks
// alike, and `repair --upgrade` turns one into the other.
func SameLayout(a, b Manifest) bool {
	a.Layout.Storage, b.Layout.Storage = sameStorage(a.Layout.Storage), sameStorage(b.Layout.Storage)
	ja, _ := json.Marshal(struct {
		L Layout
		E Encryption
	}{a.Layout, a.Encryption})
	jb, _ := json.Marshal(struct {
		L Layout
		E Encryption
	}{b.Layout, b.Encryption})
	return string(ja) == string(jb)
}

// Marshal encodes m as indented JSON.
func (m Manifest) Marshal() ([]byte, error) {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

// Parse decodes and validates a manifest.
func Parse(b []byte) (Manifest, error) {
	var m Manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return Manifest{}, fmt.Errorf("manifest: %w", err)
	}
	if err := m.Validate(); err != nil {
		return Manifest{}, err
	}
	return m, nil
}

// ManifestPath and MirrorPath locate the manifest files of the data root.
func ManifestPath(root string) string { return filepath.Join(root, ManifestName) }
func MirrorPath(root string) string {
	return filepath.Join(root, cnst.KVDBDIR, MirrorName)
}

// ErrNoManifest reports a data root without a manifest file.
var ErrNoManifest = errors.New("no manifest")

// Load reads the manifest of root: the root file, or the mirror when the root
// file is missing or unreadable. It returns ErrNoManifest when neither exists.
func Load(root string) (Manifest, error) {
	var firstErr error
	for _, p := range []string{ManifestPath(root), MirrorPath(root)} {
		b, err := os.ReadFile(p)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err == nil {
			var m Manifest
			if m, err = Parse(b); err == nil {
				return m, nil
			}
			if errors.Is(err, ErrNewerFormat) {
				return Manifest{}, err
			}
		}
		if firstErr == nil {
			firstErr = fmt.Errorf("%s: %w", p, err)
		}
	}
	if firstErr != nil {
		return Manifest{}, firstErr
	}
	return Manifest{}, ErrNoManifest
}

// Save writes the manifest file and its mirror atomically.
func Save(root string, m Manifest) error {
	b, err := m.Marshal()
	if err != nil {
		return err
	}
	if err := fsx.WriteFileAtomic(ManifestPath(root), b, cnst.FilePerm); err != nil {
		return err
	}
	return fsx.WriteFileAtomic(MirrorPath(root), b, cnst.FilePerm)
}

// sameStorage maps container-bidx to container for SameLayout.
func sameStorage(s string) string {
	if s == StorageContainerBIDX {
		return StorageContainer
	}
	return s
}

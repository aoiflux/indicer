package duesarc

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// A database package is a .duesarc whose manifest is version 3, kind
// database: a copy of a DUES data root's files under db/, each with its size
// and SHA-256 in the manifest.
const (
	ManifestName    = "manifest.json"
	VersionDatabase = "v3"
	KindDatabase    = "database"
	PipelineExport  = "export"
	DatabaseDir     = "db"

	// MaxManifestBytes bounds the manifest a reader decodes.
	MaxManifestBytes = 64 << 20
)

// DatabaseManifest is a database package's manifest.json. Fields added
// within version 3 are optional, so a reader ignores what it does not know.
type DatabaseManifest struct {
	Version      string       `json:"version"`
	Kind         string       `json:"kind"`
	Pipeline     string       `json:"pipeline"`
	CreatedAtUTC string       `json:"created_at_utc"`
	DbPath       string       `json:"db_path"`
	CreatedBy    Creator      `json:"created_by"`
	Database     DatabaseInfo `json:"database"`
	// Stats is left out for an encrypted database: manifest.json is
	// readable without the password.
	Stats *Stats `json:"stats,omitempty"`
	// Dirs lists the directories, so empty ones come back too.
	Dirs  []string `json:"dirs,omitempty"`
	Files []File   `json:"files"`
	// ContentSHA256 identifies the contents: ContentHash of Files. It
	// leaves out times and the host, so two packages of one database state
	// share it.
	ContentSHA256 string     `json:"content_sha256"`
	Host          *Host      `json:"host,omitempty"`
	Signature     *Signature `json:"signature,omitempty"`
}

// Creator is the program that made a package.
type Creator struct {
	Product string `json:"product"`
	Version string `json:"version"`
	Commit  string `json:"commit,omitempty"`
}

// DatabaseInfo describes the packaged database: what dues-manifest.json in
// it says, copied here so that inspecting a package reads nothing else.
type DatabaseInfo struct {
	Format        string `json:"format"`
	FormatVersion int    `json:"format_version"`
	// Components holds the versions of the parts that have their own:
	// graph_schema when the package holds a graph.
	Components map[string]int `json:"components,omitempty"`
	Layout     Layout         `json:"layout"`
	Encryption string         `json:"encryption"`
	Encrypted  bool           `json:"encrypted"`
	// PlaintextComponents names what the package carries unencrypted from
	// an encrypted database: fts and graph are never encrypted on disk.
	PlaintextComponents []string `json:"plaintext_components,omitempty"`
}

// Layout is the database's layout, as its manifest records it.
type Layout struct {
	ChunkKiB          int    `json:"chunk_kib"`
	ChunkHash         string `json:"chunk_hash"`
	Storage           string `json:"storage"`
	BlobEncoding      string `json:"blob_encoding"`
	ValueEncoding     string `json:"value_encoding"`
	BadgerCompression string `json:"badger_compression"`
}

// Stats counts what the database holds.
type Stats struct {
	Evidence     int   `json:"evidence"`
	LogicalBytes int64 `json:"logical_bytes"`
}

// File is one packaged file, its path relative to db/, slash-separated.
type File struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// Host is where a package was made, recorded only when asked for.
type Host struct {
	OS       string `json:"os"`
	Arch     string `json:"arch"`
	Hostname string `json:"hostname,omitempty"`
}

// ErrNewerPackage reports a package made by a newer DUES.
var ErrNewerPackage = errors.New("the package was made by a newer version of DUES; upgrade DUES to read it")

// ContentHash is the SHA-256 of one line per file, "path\0size\0sha256\n",
// in the order given.
func ContentHash(files []File) string {
	h := sha256.New()
	for _, f := range files {
		_, _ = io.WriteString(h, f.Path+"\x00"+strconv.FormatInt(f.Size, 10)+"\x00"+f.SHA256+"\n")
	}
	return hex.EncodeToString(h.Sum(nil))
}

// Bytes is the size of every file together.
func (m DatabaseManifest) Bytes() int64 {
	var n int64
	for _, f := range m.Files {
		n += f.Size
	}
	return n
}

// Header is what every .duesarc manifest has, for telling a database
// package from an archive of files.
type Header struct {
	Version string `json:"version"`
	Kind    string `json:"kind"`
}

// IsDatabase reports whether a manifest is a database package's.
func (h Header) IsDatabase() bool { return h.Kind == KindDatabase }

// ParseDatabaseManifest decodes and checks a database package's manifest.
func ParseDatabaseManifest(data []byte) (DatabaseManifest, error) {
	var hdr Header
	if err := json.Unmarshal(data, &hdr); err != nil {
		return DatabaseManifest{}, fmt.Errorf("the manifest is not valid JSON: %w", err)
	}
	if !hdr.IsDatabase() {
		return DatabaseManifest{}, errors.New("this archive holds files, not a DUES database; extract it with the compression tool")
	}
	if n, ok := versionNumber(hdr.Version); !ok {
		return DatabaseManifest{}, fmt.Errorf("unknown manifest version %q", hdr.Version)
	} else if n > 3 {
		return DatabaseManifest{}, fmt.Errorf("%w (manifest version %s; this DUES reads %s)", ErrNewerPackage, hdr.Version, VersionDatabase)
	} else if n < 3 {
		return DatabaseManifest{}, fmt.Errorf("manifest version %s cannot hold a database", hdr.Version)
	}
	var m DatabaseManifest
	if err := json.Unmarshal(data, &m); err != nil {
		return DatabaseManifest{}, fmt.Errorf("the manifest is not valid: %w", err)
	}
	return m, m.validate()
}

func versionNumber(v string) (int, bool) {
	n, err := strconv.Atoi(strings.TrimPrefix(v, "v"))
	return n, err == nil && strings.HasPrefix(v, "v") && n > 0
}

func (m DatabaseManifest) validate() error {
	if m.DbPath != DatabaseDir {
		return fmt.Errorf("the manifest puts the database at %q, not %q", m.DbPath, DatabaseDir)
	}
	if len(m.Files) == 0 {
		return errors.New("the manifest lists no files")
	}
	for i, f := range m.Files {
		if err := checkPath(f.Path); err != nil {
			return fmt.Errorf("manifest file %q: %w", f.Path, err)
		}
		if i > 0 && m.Files[i-1].Path >= f.Path {
			return fmt.Errorf("manifest file %q is out of order or listed twice", f.Path)
		}
		if f.Size < 0 {
			return fmt.Errorf("manifest file %q has a negative size", f.Path)
		}
		if b, err := hex.DecodeString(f.SHA256); err != nil || len(b) != sha256.Size {
			return fmt.Errorf("manifest file %q has no valid SHA-256", f.Path)
		}
	}
	for _, d := range m.Dirs {
		if err := checkPath(d); err != nil {
			return fmt.Errorf("manifest directory %q: %w", d, err)
		}
	}
	if got := ContentHash(m.Files); got != m.ContentSHA256 {
		return errors.New("the manifest's content hash does not match its file list")
	}
	return nil
}

// checkPath refuses a path that is not a plain relative one LocalName
// accepts as written: no "." or empty elements, nothing it would rewrite.
func checkPath(p string) error {
	local, err := LocalName(p)
	switch {
	case err != nil:
		return err
	case local == "":
		return errors.New("an empty path")
	}
	for c := range strings.SplitSeq(p, "/") {
		if c == "" || c == "." {
			return errors.New("not a clean relative path")
		}
	}
	return nil
}

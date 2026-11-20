package duesarc

import (
	"encoding/binary"
	"strconv"
)

const (
	SignatureScheme = "ed25519"
	// StatementDomain separates package signatures from anything else a
	// DUES key signs.
	StatementDomain = "dues.package.v1"
)

// Signature is an actor's signature over a package's Statement. Its public
// key travels with the package, so only a key held elsewhere (verify
// --trusted-key) shows who signed it.
type Signature struct {
	Scheme     string `json:"scheme"`
	Actor      string `json:"actor"`
	ActorID    string `json:"actor_id"`
	KeyID      string `json:"key_id"`
	PublicKey  []byte `json:"public_key"`
	Protection string `json:"key_protection"`
	Sig        []byte `json:"signature"`
}

// Statement is the bytes a package signature covers: length-prefixed fields
// in a fixed order. Fields are listed explicitly rather than taken from the
// manifest's JSON, so a package from a later version, with fields this one
// does not know, still verifies. The files are covered by ContentSHA256.
func (m DatabaseManifest) Statement() []byte {
	var s Signature
	if m.Signature != nil {
		s = *m.Signature
	}
	db, l := m.Database, m.Database.Layout
	stats := ""
	if m.Stats != nil {
		stats = strconv.Itoa(m.Stats.Evidence) + " " + strconv.FormatInt(m.Stats.LogicalBytes, 10)
	}
	var b []byte
	field := func(v string) {
		b = binary.BigEndian.AppendUint32(b, uint32(len(v)))
		b = append(b, v...)
	}
	list := func(vs []string) {
		field(strconv.Itoa(len(vs)))
		for _, v := range vs {
			field(v)
		}
	}
	field(StatementDomain)
	field(s.Scheme)
	field(m.Version)
	field(m.Kind)
	field(m.Pipeline)
	field(m.CreatedAtUTC)
	field(m.DbPath)
	field(m.ContentSHA256)
	list(m.Dirs)
	field(stats)
	field(db.Format)
	field(strconv.Itoa(db.FormatVersion))
	field(strconv.Itoa(db.Components["graph_schema"]))
	field(db.Encryption)
	field(strconv.FormatBool(db.Encrypted))
	list(db.PlaintextComponents)
	field(strconv.Itoa(l.ChunkKiB))
	field(l.ChunkHash)
	field(l.Storage)
	field(l.BlobEncoding)
	field(l.ValueEncoding)
	field(l.BadgerCompression)
	field(s.Actor)
	field(s.ActorID)
	field(s.KeyID)
	field(string(s.PublicKey))
	field(s.Protection)
	return b
}

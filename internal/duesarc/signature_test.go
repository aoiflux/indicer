package duesarc

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

// TestStatementIsStable pins the statement's bytes: a change would break
// the signature of every package signed before it.
func TestStatementIsStable(t *testing.T) {
	t.Parallel()
	m := validManifest()
	m.CreatedAtUTC = "2026-11-10T00:00:00Z"
	m.Dirs = []string{"fts/empty"}
	m.Stats = &Stats{Evidence: 1, LogicalBytes: 307200}
	m.Database = DatabaseInfo{
		Format: "dues", FormatVersion: 1, Components: map[string]int{"graph_schema": 3},
		Layout:     Layout{ChunkKiB: 256, ChunkHash: "blake3", Storage: "file", BlobEncoding: "zstd", ValueEncoding: "v2", BadgerCompression: "none"},
		Encryption: "none",
	}
	m.Signature = &Signature{
		Scheme: SignatureScheme, Actor: "database", ActorID: "0123456789abcdef", KeyID: "fedcba9876543210",
		PublicKey: make([]byte, 32), Protection: "none", Sig: []byte("not covered"),
	}
	sum := sha256.Sum256(m.Statement())
	const want = "96dd239051515e0d946e8c6dc59b284bc4cde832cd2451d5e30c265986de1ce5"
	if got := hex.EncodeToString(sum[:]); got != want {
		t.Fatalf("statement sha256 = %s, want %s", got, want)
	}

	m.Signature.Sig = []byte("other")
	m.CreatedBy = Creator{Product: "x"}
	m.Host = &Host{OS: "plan9"}
	if again := sha256.Sum256(m.Statement()); again != sum {
		t.Error("the statement covers the signature bytes, created_by or the host")
	}
}

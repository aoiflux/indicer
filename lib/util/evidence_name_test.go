package util

import (
	"bytes"
	"encoding/base64"
	"testing"

	"indicer/lib/cnst"
)

// TestGetEvidenceFileHashAcceptsRawPrefix is the ING-19 regression. The
// parser names an indexed file by the raw 16-byte evidence ID, while
// partition names carry it base64-encoded; decoding only base64 made every
// read of an indexed file fail ("illegal base64 data").
func TestGetEvidenceFileHashAcceptsRawPrefix(t *testing.T) {
	t.Parallel()
	sep := cnst.DataSeperator
	partHash := base64.StdEncoding.EncodeToString([]byte("partition-hash-0"))
	ids := map[string][]byte{
		"plain":           []byte("0123456789abcdef"),
		"separator bytes": []byte("ab|||cd|||ef||||"),
		"high bytes":      {0xff, 0x00, 0x7c, 0x7c, 0x7c, 0x80, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10},
	}
	for name, id := range ids {
		t.Run(name, func(t *testing.T) {
			raw := string(id) + sep + partHash + sep + "dir/file.txt"
			got, err := GetEvidenceFileHash(raw)
			if err != nil || !bytes.Equal(got, id) {
				t.Fatalf("raw-prefixed name: got %x, %v; want %x", got, err, id)
			}
			encoded := base64.StdEncoding.EncodeToString(id) + sep + "image.dd_p0"
			got, err = GetEvidenceFileHash(encoded)
			if err != nil || !bytes.Equal(got, id) {
				t.Fatalf("base64-prefixed name: got %x, %v; want %x", got, err, id)
			}
		})
	}

	// A legacy evidence ID that is a 32-byte content hash, base64-encoded.
	long := bytes.Repeat([]byte{0xab}, 32)
	got, err := GetEvidenceFileHash(base64.StdEncoding.EncodeToString(long) + sep + "p")
	if err != nil || !bytes.Equal(got, long) {
		t.Fatalf("long base64 prefix: got %x, %v", got, err)
	}

	for _, bad := range []string{"", "not base64!" + sep + "x", "short|||x"} {
		if got, err := GetEvidenceFileHash(bad); err == nil {
			t.Fatalf("GetEvidenceFileHash(%q) = %x, want an error", bad, got)
		}
	}
}

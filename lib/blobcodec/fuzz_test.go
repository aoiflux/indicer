package blobcodec

import (
	"bytes"
	"testing"
)

// FuzzDecode feeds every codec stored bytes it did not write: the result is
// an error, never a panic. Content any codec encodes decodes back to itself.
func FuzzDecode(f *testing.F) {
	for _, c := range codecs(f) {
		enc, err := c.Encode(blakeKey([]byte("seed chunk")), []byte("seed chunk"))
		if err != nil {
			f.Fatal(err)
		}
		f.Add(enc, []byte("seed chunk"))
	}
	f.Add([]byte{}, []byte{})
	f.Fuzz(func(t *testing.T, stored, plain []byte) {
		ckey := blakeKey(plain)
		for name, c := range codecs(t) {
			if got, err := c.Decode(ckey, stored); err == nil && name != "legacy-raw" && name != "legacy-sealed" {
				// Codecs that check content return only content of the key.
				if Verify(ckey, got) != nil {
					t.Fatalf("%s decoded %d bytes that do not match their key", name, len(got))
				}
			}
			enc, err := c.Encode(ckey, plain)
			if err != nil {
				t.Fatalf("%s: encode: %v", name, err)
			}
			dec, err := c.Decode(ckey, enc)
			if err != nil || !bytes.Equal(dec, plain) {
				t.Fatalf("%s: round trip of %d bytes: %v", name, len(plain), err)
			}
		}
	})
}

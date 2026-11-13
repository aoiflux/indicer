package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"indicer/lib/dbopen"
)

// FuzzOriginCheck: an origin the server lets call it normalises, and
// normalising is stable; nothing panics.
func FuzzOriginCheck(f *testing.F) {
	for _, s := range []string{"https://case.example", "http://localhost:3000", "http://[::1]:8080", "null", "https://case.example:443/", "javascript:alert(1)", "http://user@127.0.0.1"} {
		f.Add(s)
	}
	s := &Server{origins: map[string]bool{"https://case.example": true}}
	f.Fuzz(func(t *testing.T, origin string) {
		if n, ok := normalizeOrigin(origin); ok {
			if again, ok2 := normalizeOrigin(n); !ok2 || again != n {
				t.Fatalf("%q normalised to %q, then to %q (%v)", origin, n, again, ok2)
			}
		}
		if s.originAllowed(origin) {
			if _, ok := normalizeOrigin(origin); !ok {
				t.Fatalf("%q allowed but not an origin", origin)
			}
		}
	})
}

// FuzzParseTokens: a token file either gives credentials the server takes,
// or is refused with an error that quotes nothing a token could be.
func FuzzParseTokens(f *testing.F) {
	tok := strings.Repeat("a", MinTokenLength)
	for _, s := range []string{
		tok, "\xEF\xBB\xBF" + tok + "\r\n", "#" + tok, tok + "\nsecond",
		TokenFileHeader + "\n" + tok + " read,upload alice\n# off\n" + tok + "b== all bob\n",
		TokenFileHeader + "\nalice read " + tok, TokenFileHeader + "\n" + tok + " read " + tok + "x",
		TokenFileHeader + "\n" + tok + " read Alice\n" + tok + "b read alice", "#dues-tokens v2\n" + tok + " read a",
		TokenFileHeader + "\nalice " + tok + " read", TokenFileHeader + " x\n" + tok + " read a", "\n" + tok,
		TokenFileHeader + "\n" + strings.Repeat("=", MinTokenLength) + " read a",
	} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		creds, err := ParseTokens(data)
		if err != nil {
			for field := range strings.FieldsSeq(string(data)) {
				if len(field) >= MinTokenLength && strings.Contains(err.Error(), field) {
					t.Fatalf("the error quotes the file: %v", err)
				}
			}
			return
		}
		if len(creds) == 0 || len(creds) > MaxTokens {
			t.Fatalf("%d credentials", len(creds))
		}
		if err := (Config{Tokens: creds}).Validate(); err != nil {
			t.Fatalf("the credentials do not validate: %v", err)
		}
	})
}

// FuzzUploadRequests sends upload start bodies and chunk offsets the server
// did not expect: each gets a 2xx or 4xx answer, or 507 for space, never a
// panic or another 5xx.
func FuzzUploadRequests(f *testing.F) {
	h, err := dbopen.Open(context.Background(), dbopen.Request{Path: filepath.Join(f.TempDir(), "db"), Mode: dbopen.ReadWrite})
	if err != nil {
		f.Fatal(err)
	}
	f.Cleanup(func() { _ = h.Close() })
	s, err := New(Config{Limits: testLimits()}, Deps{DB: h.DB, Root: h.Root})
	if err != nil {
		f.Fatal(err)
	}
	f.Add([]byte(`{"file_path":"/a.bin","file_hash":"AAAA","size":4}`), "0", []byte("abcd"))
	f.Add([]byte(`{"file_path":"/a.bin","size":-1}`), "-5", []byte{})
	f.Add([]byte(`{"size":1e30}`), "99999999999999999999", []byte("x"))
	f.Add([]byte(`not json`), "", []byte("x"))
	okStatus := func(code int) bool { return code < 500 || code == http.StatusInsufficientStorage }
	f.Fuzz(func(t *testing.T, start []byte, offset string, chunk []byte) {
		w := httptest.NewRecorder()
		s.handleUploadStart(w, httptest.NewRequest(http.MethodPost, "/web/upload/start", bytes.NewReader(start)))
		if !okStatus(w.Code) {
			t.Fatalf("start %q: %d %s", start, w.Code, w.Body)
		}
		var started struct {
			UploadID string `json:"upload_id"`
		}
		if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &started) != nil || started.UploadID == "" {
			return
		}
		defer func() {
			body := strings.NewReader(`{"upload_id":"` + started.UploadID + `"}`)
			s.handleUploadAbort(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/web/upload/abort", body))
		}()
		r := httptest.NewRequest(http.MethodPost, "/web/upload/chunk", bytes.NewReader(chunk))
		r.Header.Set("Upload-Id", started.UploadID)
		r.Header.Set("Upload-Offset", offset)
		w = httptest.NewRecorder()
		s.handleUploadChunk(w, r)
		if !okStatus(w.Code) {
			t.Fatalf("chunk at %q: %d %s", offset, w.Code, w.Body)
		}
	})
}

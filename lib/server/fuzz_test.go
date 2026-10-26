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

	"indicer/internal/testutil/testdb"
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

// FuzzUploadRequests sends upload start bodies and chunk offsets the server
// did not expect: each gets a 2xx or 4xx answer, or 507 for space, never a
// panic or another 5xx.
func FuzzUploadRequests(f *testing.F) {
	testdb.Isolate(f)
	testdb.EnsureCodecs(f)
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

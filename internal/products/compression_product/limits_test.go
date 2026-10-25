package compression_product

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/klauspost/compress/zstd"

	coreerr "indicer/internal/core/errors"
)

// zeroStream returns a zstd stream that decodes to n zero bytes.
func zeroStream(t *testing.T, n int) []byte {
	t.Helper()
	var buf bytes.Buffer
	w, err := zstd.NewWriter(&buf, zstd.WithEncoderLevel(zstd.SpeedFastest))
	if err != nil {
		t.Fatal(err)
	}
	zeros := make([]byte, 1<<20)
	for left := n; left > 0; left -= len(zeros) {
		if _, err := w.Write(zeros[:min(left, len(zeros))]); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func handle(svc *Service, op string, params map[string]any) (any, *coreerr.Error) {
	raw, _ := json.Marshal(params)
	return svc.Handle(context.Background(), op, raw)
}

// TestDecompressBytesIsBounded is the CMP-1 regression for decompress_bytes:
// a few kilobytes decoded to whatever their frames declared.
func TestDecompressBytesIsBounded(t *testing.T) {
	svc := New()
	bomb := base64.StdEncoding.EncodeToString(zeroStream(t, 80<<20))
	if _, rerr := handle(svc, opDecompressBytes, map[string]any{"data_base64": bomb}); rerr == nil || rerr.Code != coreerr.CodeLimitExceeded {
		t.Fatalf("80 MiB with the default limit: %v, want limit_exceeded", rerr)
	}
	res, rerr := handle(svc, opDecompressBytes, map[string]any{"data_base64": bomb, "max_output_bytes": 100 << 20})
	if rerr != nil || res.(decompressRes).OriginalBytes != 80<<20 {
		t.Fatalf("80 MiB with a 100 MiB limit: %v", rerr)
	}
	if _, rerr := handle(svc, opDecompressBytes, map[string]any{"data_base64": bomb, "max_output_bytes": 1 << 30}); rerr == nil || rerr.Code != coreerr.CodeInvalidRequest {
		t.Fatalf("a 1 GiB limit: %v, want invalid_request", rerr)
	}
	if _, rerr := handle(svc, opDecompressBytes, map[string]any{"data_base64": base64.StdEncoding.EncodeToString([]byte("not zstd"))}); rerr == nil || rerr.Code != coreerr.CodeInvalidRequest {
		t.Fatalf("garbage: %v, want invalid_request", rerr)
	}
}

func TestDecompressFileIsBounded(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "bomb.zst")
	if err := os.WriteFile(in, zeroStream(t, 20<<20), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "bomb.out")
	svc := New()
	_, rerr := handle(svc, opDecompressFile, map[string]any{"input_path": in, "output_path": out, "limits": map[string]any{"max_total_bytes": 10 << 20}})
	if rerr == nil || rerr.Code != coreerr.CodeLimitExceeded || rerr.Details["limit"] != "max_total_bytes" {
		t.Fatalf("20 MiB with a 10 MiB limit: %v %v", rerr, rerr.Details)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Fatalf("a refused decompression left %v", entries)
	}
	res, rerr := handle(svc, opDecompressFile, map[string]any{"input_path": in, "output_path": out})
	if rerr != nil || res.(fileRes).OriginalBytes != 20<<20 {
		t.Fatalf("20 MiB with the default limit: %v", rerr)
	}
	if _, rerr := handle(svc, opDecompressFile, map[string]any{"input_path": in, "limits": map[string]any{"max_entries": -1}}); rerr == nil || rerr.Code != coreerr.CodeInvalidRequest {
		t.Fatalf("a negative limit: %v", rerr)
	}
}

func TestOutputGuardRatio(t *testing.T) {
	limits, _ := Limits{MaxRatio: 100}.resolve()
	in := int64(10)
	g := &outputGuard{limits: limits, in: func() int64 { return in }, grace: 1000}
	if err := g.add(1000); err != nil {
		t.Fatalf("within the grace: %v", err)
	}
	if err := g.add(100); err == nil {
		t.Fatal("1100 bytes out of 10 passed a ratio of 100")
	}
	g = &outputGuard{limits: limits, in: func() int64 { return 1000 }, grace: 1000}
	if err := g.add(50_000); err != nil {
		t.Fatalf("a ratio of 50: %v", err)
	}
}

// TestConcurrentArchiveOperations: archives of different layouts created and
// extracted at once in one process each come back intact (FFI-2). Under
// -race, it also catches two databases' layouts being set at once.
func TestConcurrentArchiveOperations(t *testing.T) {
	root := t.TempDir()
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			src := filepath.Join(root, fmt.Sprintf("src%d", i))
			want := map[string]string{"a.txt": fmt.Sprintf("file %d ", i) + string(bytes.Repeat([]byte{byte(i)}, 300_000)), "b/c.txt": "c"}
			for rel, content := range want {
				p := filepath.Join(src, filepath.FromSlash(rel))
				_ = os.MkdirAll(filepath.Dir(p), 0o755)
				if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
					errs <- err
					return
				}
			}
			chunk := []int{64, 1024}[i%2]
			archive := filepath.Join(root, fmt.Sprintf("a%d.duesarc", i))
			svc := New()
			if _, rerr := handle(svc, opCreateArchive, map[string]any{"input_path": src, "output_path": archive, "chunk_size_kb": chunk}); rerr != nil {
				errs <- fmt.Errorf("create %d: %v %v", i, rerr, rerr.Details)
				return
			}
			dst := filepath.Join(root, fmt.Sprintf("out%d", i))
			if _, rerr := handle(svc, opExtractArchive, map[string]any{"input_path": archive, "output_path": dst}); rerr != nil {
				errs <- fmt.Errorf("extract %d: %v %v", i, rerr, rerr.Details)
				return
			}
			got, err := os.ReadFile(filepath.Join(dst, "a.txt"))
			if err != nil || string(got) != want["a.txt"] {
				errs <- fmt.Errorf("archive %d: a.txt came back wrong (%d bytes, %v)", i, len(got), err)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

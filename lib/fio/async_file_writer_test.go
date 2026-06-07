package fio

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestAsyncFileWriterDrainsAndWritesAll(t *testing.T) {
	t.Helper()

	StartAsyncFileWriter(4)

	tmpDir := t.TempDir()
	const total = 200

	for i := 0; i < total; i++ {
		path := filepath.Join(tmpDir, fmt.Sprintf("chunk-%03d.blob", i))
		data := []byte(fmt.Sprintf("payload-%03d", i))
		if err := EnqueueAsyncFileWrite(path, data); err != nil {
			t.Fatalf("enqueue %d failed: %v", i, err)
		}
	}

	if err := CloseAsyncFileWriter(); err != nil {
		t.Fatalf("close async writer failed: %v", err)
	}

	for i := 0; i < total; i++ {
		path := filepath.Join(tmpDir, fmt.Sprintf("chunk-%03d.blob", i))
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("missing written file %q: %v", path, err)
		}
		want := fmt.Sprintf("payload-%03d", i)
		if string(b) != want {
			t.Fatalf("unexpected content for %q: got=%q want=%q", path, string(b), want)
		}
	}
}

func TestAsyncFileWriterReturnsExistAsNil(t *testing.T) {
	t.Helper()

	StartAsyncFileWriter(1)

	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "same.blob")
	data := []byte("same-data")

	if err := EnqueueAsyncFileWrite(path, data); err != nil {
		t.Fatalf("first enqueue failed: %v", err)
	}
	if err := EnqueueAsyncFileWrite(path, data); err != nil {
		t.Fatalf("second enqueue failed: %v", err)
	}

	if err := CloseAsyncFileWriter(); err != nil {
		t.Fatalf("close async writer failed: %v", err)
	}

	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read file failed: %v", err)
	}
	if string(b) != "same-data" {
		t.Fatalf("unexpected content: got=%q", string(b))
	}
}

func TestAsyncFileWriterRejectsEnqueueAfterClose(t *testing.T) {
	t.Helper()

	StartAsyncFileWriter(1)
	if err := CloseAsyncFileWriter(); err != nil {
		t.Fatalf("close async writer failed: %v", err)
	}

	err := EnqueueAsyncFileWrite(filepath.Join(t.TempDir(), "x.blob"), []byte("x"))
	if err == nil {
		t.Fatal("expected enqueue after close to fail")
	}
}

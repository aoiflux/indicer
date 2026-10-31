package store

import (
	"bytes"
	"context"
	"os"
	"testing"

	"indicer/internal/lifecycle"
	"indicer/internal/progress"
	"indicer/internal/testutil"
	"indicer/internal/testutil/testdb"
	"indicer/lib/blobcodec"
	"indicer/lib/util"
)

// TestChunkPassProgress: both pipelines report the bytes they read on one
// "store" phase, the bytes they stored new on its meter and on the
// command's tallies, and the same counts in ChunkResult. A second pass of
// the same content stores nothing new.
func TestChunkPassProgress(t *testing.T) {
	for name, pass := range chunkPasses {
		t.Run(name, func(t *testing.T) {
			db := testdb.Open(t, testdb.Default)
			util.SetChonkSize(4)
			if err := util.EnsureBlobPath(db.DB.Opts().Dir); err != nil {
				t.Fatal(err)
			}
			unique := testutil.RandomBytes(5, 64<<10)
			content := append(append([]byte{}, unique...), unique...) // the second half repeats the first

			r := progress.New(t.Context(), nil)
			ctx := progress.With(t.Context(), r)
			res, err := pass(ctx, ChunkPass{DB: db.DB, ID: []byte("e1"), Source: byteReaderAt(content), Size: int64(len(content))})
			if err != nil {
				t.Fatal(err)
			}
			snap := r.Snapshot(0)
			if len(snap.Phases) != 1 {
				t.Fatalf("phases %+v", snap.Phases)
			}
			v := snap.Phases[0]
			if v.Name != "store" || v.State != progress.OK || v.Done != int64(len(content)) || v.Total != int64(len(content)) {
				t.Fatalf("store phase %+v", v)
			}
			if res.NewBytes != int64(len(unique)) || res.NewChunks != int64(len(unique)/4096) {
				t.Fatalf("new %d bytes in %d chunks, want %d in %d", res.NewBytes, res.NewChunks, len(unique), len(unique)/4096)
			}
			if len(v.Meters) != 1 || v.Meters[0].Value != res.NewBytes {
				t.Fatalf("meters %+v", v.Meters)
			}
			tallies := map[string]int64{}
			for _, m := range snap.Tallies {
				tallies[m.Name] = m.Value
			}
			if tallies["read"] != int64(len(content)) || tallies["new"] != res.NewBytes {
				t.Fatalf("tallies %v", tallies)
			}

			again, err := pass(t.Context(), ChunkPass{DB: db.DB, ID: []byte("e2"), Source: byteReaderAt(content), Size: int64(len(content)), Quiet: true})
			if err != nil || again.NewBytes != 0 || again.NewChunks != 0 {
				t.Fatalf("second pass: %+v, %v; want nothing new", again, err)
			}
		})
	}
}

func TestChunkPassQuietReportsNothing(t *testing.T) {
	db := testdb.Open(t, testdb.Default)
	if err := util.EnsureBlobPath(db.DB.Opts().Dir); err != nil {
		t.Fatal(err)
	}
	r := progress.New(t.Context(), nil)
	content := testutil.RandomBytes(6, 1<<16)
	if _, err := IngestChunks(progress.With(t.Context(), r), ChunkPass{DB: db.DB, ID: []byte("q"), Source: byteReaderAt(content), Size: int64(len(content)), Quiet: true}); err != nil {
		t.Fatal(err)
	}
	if snap := r.Snapshot(0); len(snap.Phases) != 0 || len(snap.Tallies) != 0 {
		t.Fatalf("a quiet pass reported %+v", snap)
	}
}

// TestChunkPassInterrupted: a pass stopped by a signal ends interrupted,
// short of its total, never full.
func TestChunkPassInterrupted(t *testing.T) {
	for name, pass := range chunkPasses {
		t.Run(name, func(t *testing.T) {
			db := testdb.Open(t, testdb.Default)
			if err := util.EnsureBlobPath(db.DB.Opts().Dir); err != nil {
				t.Fatal(err)
			}
			root, cancel := context.WithCancelCause(t.Context())
			cancel(&lifecycle.SignalError{Signal: os.Interrupt})
			r := progress.New(root, nil)
			content := testutil.RandomBytes(7, 1<<20)
			if _, err := pass(progress.With(root, r), ChunkPass{DB: db.DB, ID: []byte("i"), Source: byteReaderAt(content), Size: int64(len(content))}); err == nil {
				t.Fatal("a cancelled pass succeeded")
			}
			v := r.Snapshot(0).Phases[0]
			if v.State != progress.Interrupted || v.Done >= v.Total {
				t.Fatalf("store phase %+v", v)
			}
		})
	}
}

// The store phase says when the chunks it writes are encrypted, and says
// nothing when they are not. The codecs are the ones dbopen registers for a
// new database with and without a password.
func TestChunkPassNotesEncryption(t *testing.T) {
	for name, key := range map[string][]byte{"unencrypted": nil, "encrypted": bytes.Repeat([]byte{7}, 32)} {
		t.Run(name, func(t *testing.T) {
			db := testdb.Open(t, testdb.Default)
			if err := util.EnsureBlobPath(db.DB.Opts().Dir); err != nil {
				t.Fatal(err)
			}
			codec, err := blobcodec.NewV2(key)
			if err != nil {
				t.Fatal(err)
			}
			blobcodec.Register(db.DB, codec)
			t.Cleanup(func() { blobcodec.Unregister(db.DB) })
			r := progress.New(t.Context(), nil)
			content := testutil.RandomBytes(7, 64<<10)
			if _, err := IngestChunks(progress.With(t.Context(), r), ChunkPass{DB: db.DB, ID: []byte("e"), Source: byteReaderAt(content), Size: int64(len(content))}); err != nil {
				t.Fatal(err)
			}
			want := ""
			if key != nil {
				want = "encrypted"
			}
			if v := r.Snapshot(0).Phases[0]; v.Note != want {
				t.Errorf("note %q, want %q", v.Note, want)
			}
		})
	}
}

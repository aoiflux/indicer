package store

import (
	"bytes"
	"context"
	"encoding/base64"
	"os"
	"testing"

	"indicer/internal/lifecycle"
	"indicer/internal/progress"
	"indicer/internal/testutil"
	"indicer/internal/testutil/testdb"
)

// TestRestoreProgress: a restore counts the bytes it writes on a "restore"
// phase and a "restored" tally; one stopped by a signal ends interrupted,
// short of its total.
func TestRestoreProgress(t *testing.T) {
	db := testdb.Open(t, testdb.Default, testdb.Options{ChunkKiB: 4})
	content := testutil.RandomBytes(21, 9*4096+11)
	rid := seedCompletedEvidence(t, db.DB, "restore-progress", content, plainHash(db.DB, content))
	id := base64.StdEncoding.EncodeToString(rid)

	r := progress.New(t.Context(), nil)
	var out bytes.Buffer
	if _, err := Restore(progress.With(t.Context(), r), db.DB, id, &out, RestoreOptions{}); err != nil {
		t.Fatal(err)
	}
	snap := r.Snapshot(0)
	if len(snap.Phases) != 1 {
		t.Fatalf("phases %+v", snap.Phases)
	}
	if v := snap.Phases[0]; v.Name != "restore" || v.State != progress.OK || v.Done != int64(len(content)) || v.Total != int64(len(content)) {
		t.Fatalf("restore phase %+v", v)
	}
	if len(snap.Tallies) != 1 || snap.Tallies[0].Name != "restored" || snap.Tallies[0].Value != int64(len(content)) {
		t.Fatalf("tallies %+v", snap.Tallies)
	}

	root, cancel := context.WithCancelCause(t.Context())
	cancel(&lifecycle.SignalError{Signal: os.Interrupt})
	r = progress.New(root, nil)
	out.Reset()
	if _, err := Restore(progress.With(root, r), db.DB, id, &out, RestoreOptions{}); err == nil {
		t.Fatal("a cancelled restore succeeded")
	}
	if ps := r.Snapshot(0).Phases; len(ps) == 1 && (ps[0].State != progress.Interrupted || ps[0].Done >= ps[0].Total) {
		t.Fatalf("cancelled restore phase %+v", ps[0])
	}
}

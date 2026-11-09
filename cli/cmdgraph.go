package cli

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"indicer/internal/console"
	"indicer/internal/ui"
	"indicer/lib/dbopen"
	"indicer/lib/graph"
	"indicer/lib/graph/signer"
	"indicer/lib/logging"

	"go.uber.org/zap"
)

// GraphCompact folds the graph's log into its image. It holds the database
// writable, so no other DUES command writes meanwhile. With requireSigned,
// every later open of the store refuses a commit no key of the keyring
// signed.
func GraphCompact(ctx context.Context, opts DBOptions, requireSigned bool) (err error) {
	h, err := Common(ctx, opts, dbopen.ReadWrite, true)
	if err != nil {
		return err
	}
	defer closeDB(h, &err)
	if !graph.Exists(h.Root) {
		return fmt.Errorf("%w at %s: dues enrich or dues micro extract builds one", graph.ErrNoGraph, h.Root)
	}
	g, err := graph.OpenWith(h.Root, graph.OpenOptions{Stages: true})
	if err != nil {
		return err
	}
	defer func() {
		if rerr := g.Release(); rerr != nil {
			err = errors.Join(err, fmt.Errorf("closing the graph: %w", rerr))
		}
	}()
	rep, err := graph.Compact(ctx, g)
	if err != nil {
		logging.GetLogger().Error("GraphCompact ERROR", zap.String("dir", rep.Dir), zap.Error(err))
		return err
	}
	logging.GetLogger().Info("GraphCompact COMPLETE", zap.String("dir", rep.Dir),
		zap.Int64("log_bytes", rep.LogBytes), zap.Int64("image_bytes", rep.ImageBytes),
		zap.Int("nodes", rep.Image.Nodes), zap.Int("edges", rep.Image.Edges),
		zap.String("segment", rep.Segment), zap.Int64("duration_ms", rep.Took.Milliseconds()))

	w := console.Out()
	fmt.Fprintf(w, "Compacted the graph at %s in %s\n", rep.Dir, ui.Duration(rep.Took))
	fmt.Fprintf(w, "  %s of log folded into a %s image: %s nodes, %s edges\n", ui.Bytes(rep.LogBytes),
		ui.Bytes(rep.ImageBytes), ui.Count(int64(rep.Image.Nodes)), ui.Count(int64(rep.Image.Edges)))
	if rep.Segment != "" {
		fmt.Fprintf(w, "  the log is kept as %s\n", rep.Segment)
	}
	if a := g.Actor(); !a.Unattributed() {
		fmt.Fprintf(w, "  the image is attested by actor %q, key %s\n", a.Name, signer.FormatID(a.KeyID))
	}
	if requireSigned {
		if err := graph.RequireSigned(g); err != nil {
			return err
		}
		logging.GetLogger().Info("GraphCompact REQUIRE_SIGNED", zap.String("dir", rep.Dir))
		fmt.Fprintln(w, "  every commit must be signed from now on")
	}
	return nil
}

// actorNamed names the actor with ID id from root's keyring, or by its ID
// when the keyring does not hold it.
func actorNamed(root string, id uint64) string {
	if ring, err := signer.NewKeystore(root).Keyring(); err == nil {
		for _, e := range ring.Entries() {
			if e.ActorID == id {
				return fmt.Sprintf("actor %q", e.Actor)
			}
		}
	}
	return "actor " + signer.FormatID(id)
}

// GraphStatus describes the graph store from its files, opening nothing.
func GraphStatus(ctx context.Context, opts DBOptions) (err error) {
	h, err := Common(ctx, opts, dbopen.ReadOnly, false)
	if err != nil {
		return err
	}
	defer closeDB(h, &err)
	st, err := graph.Inspect(h.Root)
	if err != nil {
		return err
	}

	w := console.Out()
	fmt.Fprintf(w, "Graph at %s\n", st.Dir)
	if m := st.Marker; m != nil {
		fmt.Fprintf(w, "  schema     version %d, written by %s\n", m.SchemaVersion, m.WrittenBy)
		if m.Signing.Required {
			fmt.Fprintln(w, "  signing    every commit must be signed")
		}
	} else {
		fmt.Fprintln(w, "  schema     no marker; the next writable open writes one")
	}
	switch img := st.Image; {
	case img != nil:
		fmt.Fprintf(w, "  image      %s, version %d: %s nodes, %s edges", ui.Bytes(img.FileBytes),
			img.Version, ui.Count(int64(img.NodeCount)), ui.Count(int64(img.EdgeCount)))
		if img.LastCompactUnixNano != 0 {
			fmt.Fprintf(w, ", compacted %s", time.Unix(0, img.LastCompactUnixNano).UTC().Format(time.RFC3339))
		}
		fmt.Fprintln(w)
		if img.HasSnapshotRoots {
			fmt.Fprintf(w, "  root       %s\n", hex.EncodeToString(img.SnapshotRoot[:]))
		}
		if img.HasAttestation {
			fmt.Fprintf(w, "  attested   by %s, key %s\n", actorNamed(h.Root, img.AttestActorID), signer.FormatID(img.AttestKeyID))
		}
		if st.Marker != nil && st.Marker.Image == nil {
			fmt.Fprintln(w, "  note       the marker does not record the image yet; the next writable open records it")
		}
	case st.ImageErr != nil:
		fmt.Fprintf(w, "  image      missing: %v\n", st.ImageErr)
	default:
		fmt.Fprintln(w, "  image      none: every open replays the whole log; dues graph compact makes one")
	}
	l := st.Log
	fmt.Fprintf(w, "  log        %s, segment %d: %s records, %s commits, %s signed", ui.Bytes(l.Bytes), l.Segment,
		ui.Count(int64(l.Records)), ui.Count(int64(l.Commits)), ui.Count(int64(l.Signed)))
	if l.Loose > 0 {
		fmt.Fprintf(w, ", %s outside a commit", ui.Count(int64(l.Loose)))
	}
	fmt.Fprintln(w)
	if l.Truncated {
		fmt.Fprintln(w, "  note       the log ends in a record a crash cut short; the next open drops it")
	}
	chain := "the chain holds"
	if st.ChainErr != nil {
		chain = "BROKEN: " + st.ChainErr.Error()
	}
	if len(st.Segments) == 0 {
		chain = "none"
	}
	fmt.Fprintf(w, "  segments   %d, %s: %s\n", len(st.Segments), ui.Bytes(st.SegmentBytes()), chain)
	if st.ImageErr != nil || st.ChainErr != nil {
		return errors.New("the graph store is damaged")
	}
	return nil
}

package scan

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"math/rand/v2"
	"os"
	"testing"
	"time"

	"github.com/aoiflux/libfat"

	fixture "indicer/internal/testutil/fsfixture"
	"indicer/lib/parser/core"
	"indicer/lib/parser/image"
)

// scanBytes scans an in-memory image, failing the test if the scan does not
// finish in time (a library looping on crafted metadata) and checking that
// every data extent it reports lies inside the image.
func scanBytes(t *testing.T, data []byte) (*core.ImageReport, []core.FileRecord) {
	t.Helper()
	img := image.NewRaw(bytes.NewReader(data), int64(len(data)))
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	type result struct {
		rep  *core.ImageReport
		recs []core.FileRecord
		err  error
	}
	done := make(chan result, 1)
	go func() {
		var recs []core.FileRecord
		rep, err := Scan(ctx, img, Visitor{File: func(_ core.PartitionInfo, rec core.FileRecord) error {
			recs = append(recs, rec)
			return nil
		}}, Options{})
		done <- result{rep, recs, err}
	}()
	select {
	case r := <-done:
		for _, rec := range r.recs {
			for _, e := range rec.Content.Extents {
				if e.Kind == core.ExtentData && (e.ImageOffset < 0 || e.ImageOffset+e.Length > int64(len(data))) {
					t.Errorf("%s: extent [%d, +%d) outside the %d-byte image", rec.Path, e.ImageOffset, e.Length, len(data))
				}
			}
		}
		return r.rep, r.recs
	case <-time.After(3 * time.Minute):
		t.Fatal("scan did not finish")
		return nil, nil
	}
}

func rawFixture(t testing.TB, m fixture.Manifest) []byte {
	t.Helper()
	data, err := os.ReadFile(fixture.Open(t, m))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func panicsIn(rep *core.ImageReport) int64 {
	var n int64
	if rep == nil {
		return 0
	}
	for _, p := range rep.Partitions {
		n += p.Warnings[core.IssuePanicRecovered]
	}
	return n
}

// TestCorruptImagesTruncated: every raw fixture cut at a quarter, a half and
// three quarters scans to completion: no crash, no hang, no extent past the
// end of what is left.
func TestCorruptImagesTruncated(t *testing.T) {
	if testing.Short() {
		t.Skip("scans every fixture several times")
	}
	for _, m := range fixture.All(t) {
		if m.Format != "raw" {
			continue
		}
		data := rawFixture(t, m)
		for _, frac := range []int{25, 50, 75} {
			t.Run(fmt.Sprintf("%s/%d%%", m.Name, frac), func(t *testing.T) {
				cut := len(data) * frac / 100
				rep, recs := scanBytes(t, data[:cut])
				t.Logf("%d records, %d panics contained", len(recs), panicsIn(rep))
			})
		}
	}
}

// TestCorruptImagesBitFlips: random bit flips in the blocks that hold
// anything (metadata lives there) never crash or hang a scan.
func TestCorruptImagesBitFlips(t *testing.T) {
	if testing.Short() {
		t.Skip("scans every fixture several times")
	}
	const block = 4096
	for _, m := range fixture.All(t) {
		if m.Format != "raw" {
			continue
		}
		orig := rawFixture(t, m)
		var used []int
		for off := 0; off+block <= len(orig); off += block {
			if !bytes.Equal(orig[off:off+block], make([]byte, block)) {
				used = append(used, off)
			}
		}
		if len(used) == 0 {
			continue
		}
		for _, flips := range []int{32, 2048} {
			for seed := uint64(1); seed <= 3; seed++ {
				t.Run(fmt.Sprintf("%s/%dflips/seed%d", m.Name, flips, seed), func(t *testing.T) {
					data := append([]byte(nil), orig...)
					r := rand.New(rand.NewPCG(seed, uint64(flips)))
					for i := 0; i < flips; i++ {
						off := used[r.IntN(len(used))] + r.IntN(block)
						data[off] ^= 1 << r.IntN(8)
					}
					rep, recs := scanBytes(t, data)
					t.Logf("%d records, %d panics contained", len(recs), panicsIn(rep))
				})
			}
		}
	}
}

// TestCorruptImageDirectoryCycle points a FAT32 subdirectory back at its
// grandparent. The scan must finish and report the cycle rather than loop or
// silently lose the subtree.
func TestCorruptImageDirectoryCycle(t *testing.T) {
	data := rawFixture(t, fixtureByName(t, "fat/fat32_fsagen"))
	vol, err := libfat.Open(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	var deep, two libfat.DirEntry
	err = vol.Walk(context.Background(), func(path string, _ uint32, e libfat.DirEntry) error {
		switch path {
		case "/deep":
			deep = e
		case "/deep/one/two":
			two = e
		}
		return nil
	})
	if err != nil || deep.FirstCluster == 0 || two.EntryAbsoluteOffset <= 0 {
		t.Fatalf("fixture layout: err %v deep %+v two %+v", err, deep, two)
	}
	// Short entry fields: first cluster high word at 20, low word at 26.
	ent := data[two.EntryAbsoluteOffset : two.EntryAbsoluteOffset+32]
	binary.LittleEndian.PutUint16(ent[20:], uint16(deep.FirstCluster>>16))
	binary.LittleEndian.PutUint16(ent[26:], uint16(deep.FirstCluster))

	rep, recs := scanBytes(t, data)
	if rep == nil || len(rep.Partitions) == 0 {
		t.Fatal("no partition report")
	}
	if n := rep.Partitions[0].Warnings[core.IssueCycle]; n == 0 {
		t.Fatalf("cycle not reported; warnings %v", rep.Partitions[0].Warnings)
	}
	for _, rec := range recs {
		if len(rec.Path) > 64 {
			t.Fatalf("walk followed the cycle: %s", rec.Path)
		}
	}
}

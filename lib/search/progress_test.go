package search

import (
	"testing"

	"indicer/internal/progress"
	"indicer/internal/testutil"
	"indicer/lib/structs"
)

// TestSearchProgress: a search counts every object it reads toward the
// objects it planned, never backwards, with the bytes it read on a meter.
func TestSearchProgress(t *testing.T) {
	t.Parallel()
	db := openSearchDB(t)
	for i := range 4 {
		content := testutil.RandomBytes(uint64(70+i), 300<<10)
		storeRaw(t, db, []byte{'e', byte('0' + i), 0, 0, 0, 0, 0, 0}, content, structs.IngestStateCompleted)
	}
	r := progress.New(t.Context(), nil)
	ph := progress.Start(progress.With(t.Context(), r), "search", progress.Objects, progress.Unknown)
	stop := make(chan struct{})
	backwards := make(chan int64, 1)
	go func() {
		var last int64
		for {
			select {
			case <-stop:
				return
			default:
			}
			if ps := r.Snapshot(0).Phases; len(ps) == 1 {
				if ps[0].Done < last {
					backwards <- ps[0].Done
					return
				}
				last = ps[0].Done
			}
		}
	}()
	res, err := Run(t.Context(), db, []string{"needle"}, Options{Progress: ph})
	close(stop)
	if err != nil {
		t.Fatal(err)
	}
	ph.Done(nil)
	select {
	case d := <-backwards:
		t.Fatalf("the count went back to %d", d)
	default:
	}
	v := r.Snapshot(0).Phases[0]
	if v.Total <= 0 || v.Done != v.Total || v.Total != res.Coverage.Evidence+res.Coverage.Partitions+res.Coverage.Files {
		t.Fatalf("search phase %+v, coverage %+v", v, res.Coverage)
	}
	if len(v.Meters) != 1 || v.Meters[0].Value < 4*(300<<10) {
		t.Fatalf("read meter %+v", v.Meters)
	}
}

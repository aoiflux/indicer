package dbio

import (
	"testing"

	"github.com/dgraph-io/badger/v4"
	"github.com/dgraph-io/badger/v4/options"

	"indicer/lib/cnst"
)

func TestBadgerOptions(t *testing.T) {
	t.Parallel()
	def := badger.DefaultOptions("")
	for _, low := range []bool{true, false} {
		tun := cnst.DefaultTuning()
		tun.LowResource = low
		opts := BadgerOptions(t.TempDir(), make([]byte, 32), options.ZSTD, tun)
		if opts.NumCompactors < 2 || opts.NumCompactors > 8 {
			t.Errorf("low=%v: NumCompactors %d, want 2..8", low, opts.NumCompactors)
		}
		if opts.ValueLogMaxEntries != def.ValueLogMaxEntries {
			t.Errorf("low=%v: ValueLogMaxEntries %d, want badger's default %d", low, opts.ValueLogMaxEntries, def.ValueLogMaxEntries)
		}
		if opts.NumGoroutines != tun.MaxThreads() {
			t.Errorf("low=%v: %d goroutines, want the tuning's %d", low, opts.NumGoroutines, tun.MaxThreads())
		}
		if _, ok := opts.Logger.(badgerLogger); !ok {
			t.Errorf("badger logs through %T, want the DUES log", opts.Logger)
		}
	}
}

func TestCompactValueLog(t *testing.T) {
	t.Parallel()
	opts := BadgerOptions(t.TempDir(), nil, options.None, cnst.DefaultTuning())
	opts.ValueThreshold = 16
	db, err := badger.Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	// Nothing to reclaim is not an error.
	if n, err := CompactValueLog(db); err != nil || n != 0 {
		t.Fatalf("empty database: %d rewrites, %v", n, err)
	}
	// Values large enough for the value log, then deleted: garbage.
	for i := range 2000 {
		key := []byte{byte(i >> 8), byte(i)}
		if err := db.Update(func(txn *badger.Txn) error { return txn.Set(key, make([]byte, 1024)) }); err != nil {
			t.Fatal(err)
		}
		if err := db.Update(func(txn *badger.Txn) error { return txn.Delete(key) }); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := CompactValueLog(db); err != nil {
		t.Fatalf("CompactValueLog: %v", err)
	}
}

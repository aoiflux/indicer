package fio

import (
	"testing"

	"indicer/lib/cnst"
)

// TestSetWriteEngine: the store io engine and io-uring queue depth come from
// SetWriteEngine, and an engine the process does not know reads as auto.
func TestSetWriteEngine(t *testing.T) {
	prevEngine, prevDepth := writeEngine, uringDepth
	t.Cleanup(func() { SetWriteEngine(prevEngine, prevDepth) })

	SetWriteEngine(cnst.StoreIOEngineUring, 64)
	if got := requestedEngine(); got != cnst.StoreIOEngineUring {
		t.Fatalf("requested engine %q, want %q", got, cnst.StoreIOEngineUring)
	}
	if got := GetWriteBackendStats(); got.RequestedEngine != cnst.StoreIOEngineUring || got.IOUringQueueDepth != 64 {
		t.Fatalf("stats: engine %q, depth %d; want %q, 64", got.RequestedEngine, got.IOUringQueueDepth, cnst.StoreIOEngineUring)
	}
	SetWriteEngine(cnst.StoreIOEngineUring, 0)
	if got, want := ioUringQueueDepth(), (cnst.Tuning{}).IOUringQueueDepth(); got != want {
		t.Fatalf("auto depth %d, want the machine's %d", got, want)
	}
	SetWriteEngine("no-such-engine", 0)
	if got := requestedEngine(); got != cnst.StoreIOEngineAuto {
		t.Fatalf("an unknown engine reads as %q, want %q", got, cnst.StoreIOEngineAuto)
	}
}

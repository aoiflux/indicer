package console

import "testing"

// Under go test the standard streams are pipes, not terminals.
func TestDetectUnderTest(t *testing.T) {
	if d := Detect(); d.OutTTY || d.ErrTTY || d.Shared {
		t.Fatalf("Detect() = %+v under go test", d)
	}
}

func TestSizeFallsBackToEnvironment(t *testing.T) {
	t.Setenv("COLUMNS", "132")
	t.Setenv("LINES", "50")
	if w, h := Size(); w != 132 || h != 50 {
		t.Fatalf("Size() = %d, %d; want 132, 50", w, h)
	}
	t.Setenv("COLUMNS", "")
	t.Setenv("LINES", "x")
	if w, h := Size(); w != 80 || h != 24 {
		t.Fatalf("Size() = %d, %d; want 80, 24", w, h)
	}
}

func TestSetNonInteractive(t *testing.T) {
	t.Cleanup(func() { SetNonInteractive(false) })
	SetNonInteractive(true)
	if Interactive() {
		t.Fatal("Interactive() with --ci")
	}
}

package console

import "testing"

// Under go test the standard streams are pipes, not terminals.
func TestDetectUnderTest(t *testing.T) {
	if d := Detect(); d.OutTTY || d.ErrTTY || d.Shared {
		t.Fatalf("Detect() = %+v under go test", d)
	}
}

// TestSizeIgnoresEnvironment: off a terminal the size is 80x24, whatever
// COLUMNS and LINES say.
func TestSizeIgnoresEnvironment(t *testing.T) {
	t.Setenv("COLUMNS", "132")
	t.Setenv("LINES", "50")
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

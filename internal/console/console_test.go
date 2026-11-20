package console

import (
	"bytes"
	"io"
	"testing"
)

func TestConfigureAndQuiet(t *testing.T) {
	t.Cleanup(Reset)

	var out, errw bytes.Buffer
	Configure(&out, &errw)
	Println("result")
	Printf("%d\n", 2)
	Errorln("warning")
	if got := out.String(); got != "result\n2\n" {
		t.Fatalf("out = %q", got)
	}
	if got := errw.String(); got != "warning\n" {
		t.Fatalf("err = %q", got)
	}
	if Progress() != &errw {
		t.Fatal("progress does not follow err")
	}

	out.Reset()
	errw.Reset()
	restore := Quiet()
	Println("hidden")
	Errorf("hidden %d", 1)
	restore()
	if out.Len() != 0 || errw.Len() != 0 {
		t.Fatalf("Quiet leaked %q / %q", out.String(), errw.String())
	}
	Println("back")
	if out.String() != "back\n" {
		t.Fatalf("restore did not restore: %q", out.String())
	}
}

func TestWrapAndErrSink(t *testing.T) {
	t.Cleanup(Reset)
	var out, errw, wrapped bytes.Buffer
	Configure(&out, &errw)
	restore := Wrap(func(o, e io.Writer) (io.Writer, io.Writer) {
		if o != &out || e != &errw {
			t.Fatal("Wrap did not pass the current sinks")
		}
		return o, &wrapped
	})
	if Progress() != &errw {
		t.Fatal("Wrap changed the progress sink")
	}
	_, _ = ErrSink.Write([]byte("late\n"))
	restore()
	_, _ = ErrSink.Write([]byte("back\n"))
	if wrapped.String() != "late\n" || errw.String() != "back\n" {
		t.Fatalf("wrapped %q, err %q", wrapped.String(), errw.String())
	}
}

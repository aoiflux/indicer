package console

import (
	"bytes"
	"strings"
	"testing"

	"github.com/fatih/color"
)

func TestConfigureAndQuiet(t *testing.T) {
	t.Cleanup(Reset)
	prevNoColor := color.NoColor
	color.NoColor = true
	t.Cleanup(func() { color.NoColor = prevNoColor })

	var out, errw bytes.Buffer
	Configure(&out, &errw)
	Println("result")
	Errorln("warning")
	color.Yellow("colored notice")
	color.New(color.FgRed).Println("colored result")
	if got := out.String(); !strings.Contains(got, "result") || !strings.Contains(got, "colored notice") || !strings.Contains(got, "colored result") {
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
	color.Red("hidden")
	restore()
	if out.Len() != 0 || errw.Len() != 0 {
		t.Fatalf("Quiet leaked %q / %q", out.String(), errw.String())
	}
	Println("back")
	if out.String() != "back\n" {
		t.Fatalf("restore did not restore: %q", out.String())
	}
}

package termsafe

import (
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"
)

// r and esc build non-ASCII cases from code points, keeping this file free of
// invisible characters.
func r(code rune) string   { return string(code) }
func esc(code rune) string { return fmt.Sprintf("%cu%04x", 0x5c, code) }

func TestStringEscapesControls(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"plain.txt":                            "plain.txt",
		"na" + r(0xef) + "ve.doc":              "na" + r(0xef) + "ve.doc",
		r(0x65e5) + r(0x672c) + ".txt":         r(0x65e5) + r(0x672c) + ".txt",
		"a\x1b[2Jb":                            `a\x1b[2Jb`,
		"title\x1b]0;pwned\x07":                `title\x1b]0;pwned\x07`,
		"line\nbreak\ttab\r":                   `line\x0abreak\x09tab\x0d`,
		"del\x7f":                              `del\x7f`,
		"c1" + r(0x9b) + "31m":                 "c1" + esc(0x9b) + "31m",
		"invoice" + r(0x202e) + "fdp.exe":      "invoice" + esc(0x202e) + "fdp.exe",
		"iso" + r(0x2066) + "late" + r(0x2069): "iso" + esc(0x2066) + "late" + esc(0x2069),
		"zero" + r(0x200b) + "width":           "zero" + esc(0x200b) + "width",
		"bad\xffutf8\xc3":                      `bad\xffutf8\xc3`,
	}
	for in, want := range cases {
		if got := String(in); got != want {
			t.Errorf("String(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestBlockKeepsLayout(t *testing.T) {
	t.Parallel()
	in := "line one\n\tindented\x1b[31m\r\n"
	want := "line one\n\tindented" + `\x1b` + "[31m" + `\x0d` + "\n"
	if got := Block(in); got != want {
		t.Fatalf("Block(%q) = %q, want %q", in, got, want)
	}
}

func FuzzString(f *testing.F) {
	for _, s := range []string{"", "a\x1bb", r(0x202e), "\xff\xfe", "ok\n"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		for _, out := range []string{String(s), Block(s)} {
			if !utf8.ValidString(out) {
				t.Fatalf("output is not valid UTF-8: %q", out)
			}
			for _, c := range out {
				if (c < 0x20 && c != '\n' && c != '\t') || c == 0x7f || unsafeRune(c) {
					t.Fatalf("unsafe rune %U survived in %q", c, out)
				}
			}
		}
		if strings.ContainsAny(String(s), "\n\t") {
			t.Fatalf("String kept a layout character: %q", String(s))
		}
	})
}

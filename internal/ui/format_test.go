package ui

import (
	"testing"
	"time"

	"indicer/internal/progress"
)

func TestFormat(t *testing.T) {
	cases := []struct{ got, want string }{
		{fmtBytes(0), "0 B"},
		{fmtBytes(1023), "1023 B"},
		{fmtBytes(1024), "1.00 KiB"},
		{fmtBytes(4_509_715_660), "4.20 GiB"},
		{fmtBytes(155_189_248), "148 MiB"},
		{fmtBytes(30 << 20), "30.0 MiB"},
		{fmtCount(0), "0"},
		{fmtCount(999), "999"},
		{fmtCount(182344), "182,344"},
		{fmtCount(-1234567), "-1,234,567"},
		{fmtAmount(182344, progress.Files), "182,344 files"},
		{fmtAmount(3, progress.None), "3"},
		{fmtPair(2_802_466_406, 4_509_715_660, progress.Bytes), "2.61/4.20 GiB"},
		{fmtPair(12, 340, progress.Evidence), "12/340 evidence"},
		{fmtPair(512, 900, progress.Bytes), "512/900 B"},
		{fmtRate(155_189_248, progress.Bytes), "148 MiB/s"},
		{fmtRate(3120.4, progress.Files), "3,120 files/s"},
		{fmtRate(-1, progress.Bytes), "0 B/s"},
		{fmtDuration(400 * time.Millisecond), "0.4s"},
		{fmtDuration(38200 * time.Millisecond), "38.2s"},
		{fmtDuration(12*time.Minute + 5*time.Second), "12m05s"},
		{fmtDuration(62 * time.Minute), "1h02m"},
		{fmtETA(10200 * time.Millisecond), "11s"},
		{fmtETA(0), "1s"},
		{fmtETA(125 * time.Second), "2m05s"},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("got %q, want %q", c.got, c.want)
		}
	}
	if percent(100, 100, true) != 99 || percent(100, 100, false) != 100 || percent(63, 100, true) != 63 || percent(1, 0, true) != 0 {
		t.Error("percent")
	}
}

func TestBar(t *testing.T) {
	g := GlyphsFull.Glyphs()
	var p Painter
	cases := []struct {
		done, total int64
		want        string
	}{
		{0, 100, "╸─────────"},
		{62, 100, "━━━━━━╸───"},
		{100, 100, "━━━━━━━━━━"},
		{5, 0, "╸─────────"},
	}
	for _, c := range cases {
		if got := bar(p, g, c.done, c.total, 10, Forensics); got != c.want {
			t.Errorf("bar(%d/%d) = %q, want %q", c.done, c.total, got, c.want)
		}
	}
	if bar(p, g, 1, 2, 0, Forensics) != "" {
		t.Error("zero-width bar")
	}
}

package ui

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"indicer/internal/progress"
)

// fmtBytes is n in IEC units with three significant digits: "512 B",
// "4.20 GiB", "148 MiB". Exact byte counts stay in the reports on stdout.
func fmtBytes(n int64) string {
	if n < 1024 {
		return strconv.FormatInt(n, 10) + " B"
	}
	v := float64(n)
	units := []string{"KiB", "MiB", "GiB", "TiB", "PiB", "EiB"}
	i := -1
	for v >= 1024 && i < len(units)-1 {
		v /= 1024
		i++
	}
	return sig3(v) + " " + units[i]
}

// Bytes is n as the progress display shows it, in IEC units with three
// significant digits: "512 B", "4.20 GiB".
func Bytes(n int64) string { return fmtBytes(n) }

// Count is n with thousands separators: "182,344".
func Count(n int64) string { return fmtCount(n) }

// Duration is an elapsed time as the progress display shows it: "0.4s",
// "38.2s", "12m05s".
func Duration(d time.Duration) string { return fmtDuration(d) }

// Ratio is a/b with three significant digits: "3.04x". It is "n/a" unless
// both are positive.
func Ratio(a, b int64) string {
	if a <= 0 || b <= 0 {
		return "n/a"
	}
	return sig3(float64(a)/float64(b)) + "x"
}

// Saved is how much smaller out is than in: "79.6% saved", or "grew 2.1%"
// when out is the larger ("grew <0.1%" for less). It is "n/a" when in is
// not positive.
func Saved(in, out int64) string {
	if in <= 0 {
		return "n/a"
	}
	if out > in {
		grew := float64(out-in) * 100 / float64(in)
		if grew < 0.05 {
			return "grew <0.1%"
		}
		return "grew " + strconv.FormatFloat(grew, 'f', 1, 64) + "%"
	}
	return strconv.FormatFloat(float64(in-out)*100/float64(in), 'f', 1, 64) + "% saved"
}

// sig3 formats v (1 <= v < 1024) with three significant digits.
func sig3(v float64) string {
	switch {
	case v < 10:
		return strconv.FormatFloat(v, 'f', 2, 64)
	case v < 100:
		return strconv.FormatFloat(v, 'f', 1, 64)
	}
	return strconv.FormatFloat(v, 'f', 0, 64)
}

// fmtCount is n with thousands separators: "182,344".
func fmtCount(n int64) string {
	s := strconv.FormatInt(n, 10)
	neg := strings.HasPrefix(s, "-")
	if neg {
		s = s[1:]
	}
	var b strings.Builder
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	if neg {
		return "-" + b.String()
	}
	return b.String()
}

// fmtAmount is n in unit u: bytes in IEC units, anything else counted.
func fmtAmount(n int64, u progress.Unit) string {
	switch u {
	case progress.Bytes:
		return fmtBytes(n)
	case progress.None:
		return fmtCount(n)
	}
	return fmtCount(n) + " " + string(u)
}

// fmtPair is done of total in one unit: "2.61/4.20 GiB", "12/340 evidence".
func fmtPair(done, total int64, u progress.Unit) string {
	if u != progress.Bytes {
		s := fmtCount(done) + "/" + fmtCount(total)
		if u != progress.None {
			s += " " + string(u)
		}
		return s
	}
	t := fmtBytes(total)
	unit := t[strings.LastIndexByte(t, ' ')+1:]
	scale := map[string]float64{"B": 1, "KiB": 1 << 10, "MiB": 1 << 20, "GiB": 1 << 30, "TiB": 1 << 40, "PiB": 1 << 50, "EiB": 1 << 60}[unit]
	if unit == "B" {
		return strconv.FormatInt(done, 10) + "/" + t
	}
	return sig3pad(float64(done)/scale, float64(total)/scale) + "/" + t
}

// sig3pad formats done with the decimals total uses, so the pair lines up.
func sig3pad(done, total float64) string {
	decimals := 0
	switch {
	case total < 10:
		decimals = 2
	case total < 100:
		decimals = 1
	}
	return strconv.FormatFloat(done, 'f', decimals, 64)
}

// fmtRate is a rate per second in unit u: "148 MiB/s", "3,120 files/s".
func fmtRate(perSec float64, u progress.Unit) string {
	if perSec < 0 || math.IsNaN(perSec) || math.IsInf(perSec, 0) {
		perSec = 0
	}
	if u == progress.Bytes {
		return fmtBytes(int64(perSec)) + "/s"
	}
	s := fmtCount(int64(math.Round(perSec)))
	if u != progress.None {
		s += " " + string(u)
	}
	return s + "/s"
}

// fmtDuration is an elapsed time: "0.4s", "38.2s", "12m05s", "1h02m".
func fmtDuration(d time.Duration) string {
	switch {
	case d < 0:
		d = 0
		fallthrough
	case d < time.Minute:
		return strconv.FormatFloat(d.Seconds(), 'f', 1, 64) + "s"
	case d < time.Hour:
		return fmt.Sprintf("%dm%02ds", int(d/time.Minute), int(d%time.Minute/time.Second))
	}
	return fmt.Sprintf("%dh%02dm", int(d/time.Hour), int(d%time.Hour/time.Minute))
}

// fmtETA is a remaining time in whole seconds: "11s", "2m05s", "1h02m".
func fmtETA(d time.Duration) string {
	if d < time.Minute {
		s := int(math.Ceil(d.Seconds()))
		return strconv.Itoa(max(s, 1)) + "s"
	}
	return fmtDuration(d)
}

// percent is done of total, capped at 99 while the work runs: 100% is
// shown only for work that finished.
func percent(done, total int64, running bool) int {
	if total <= 0 {
		return 0
	}
	p := int(done * 100 / total)
	if running && p > 99 {
		p = 99
	}
	return min(max(p, 0), 100)
}

// bar draws done of total in width cells: filled cells, a tip, empty
// cells. The tip shows the next cell is being filled.
func bar(p Painter, g Glyphs, done, total int64, width int, fill Role) string {
	if width <= 0 {
		return ""
	}
	n := 0
	if total > 0 {
		n = int(float64(width) * float64(min(done, total)) / float64(total))
	}
	if n >= width {
		return p.Paint(fill, strings.Repeat(g.BarFull, width))
	}
	return p.Paint(fill, strings.Repeat(g.BarFull, n)+g.BarTip) + p.Faint(strings.Repeat(g.BarEmpty, width-n-1))
}

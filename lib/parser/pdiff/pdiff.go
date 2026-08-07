// Package pdiff compares the output of the two evidence-parser backends (the
// CGo libtusk parser and the pure-Go stack) on the same image, to validate
// parity during the migration. Both backends emit the same libtusk-shaped JSON,
// so the comparison is over that shape.
package pdiff

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

type jsonFragment struct {
	Start int64 `json:"start_offset"`
	End   int64 `json:"end_offset"`
}

type jsonFile struct {
	Filename     string         `json:"filename"`
	Type         string         `json:"type"`
	IsFragmented bool           `json:"is_fragmented"`
	IsDeleted    bool           `json:"is_deleted"`
	Size         int64          `json:"size"`
	Fragments    []jsonFragment `json:"fragments"`
}

type jsonFS struct {
	Type string `json:"type"`
}

type jsonPartition struct {
	Filesystem jsonFS     `json:"filesystem"`
	Files      []jsonFile `json:"files"`
}

type jsonDoc struct {
	Partitions []jsonPartition `json:"partitions"`
	Filesystem *jsonFS         `json:"filesystem"`
	Files      []jsonFile      `json:"files"`
}

// SizeDiff records a file present in both backends with differing sizes.
type SizeDiff struct {
	Path string
	Tusk int64
	Go   int64
}

// FragDiff records a file whose fragment layout differs between backends.
type FragDiff struct {
	Path      string
	TuskRuns  int
	GoRuns    int
	TuskBytes int64
	GoBytes   int64
}

// Report is the structured result of comparing the two backends.
type Report struct {
	TuskFiles      int
	GoFiles        int
	TuskPartitions int
	GoPartitions   int
	OnlyInTusk     []string
	OnlyInGo       []string
	SizeMismatch   []SizeDiff
	FragMismatch   []FragDiff
}

// Match reports whether the two backends agree on file set, sizes, and fragment
// layout (partition counts may legitimately differ and are not part of Match).
func (r Report) Match() bool {
	return len(r.OnlyInTusk) == 0 && len(r.OnlyInGo) == 0 &&
		len(r.SizeMismatch) == 0 && len(r.FragMismatch) == 0
}

// Compare parses both backends' JSON and diffs them by file path.
func Compare(tuskJSON, goJSON string) (Report, error) {
	var td, gd jsonDoc
	if err := json.Unmarshal([]byte(tuskJSON), &td); err != nil {
		return Report{}, fmt.Errorf("parse tusk json: %w", err)
	}
	if err := json.Unmarshal([]byte(goJSON), &gd); err != nil {
		return Report{}, fmt.Errorf("parse go json: %w", err)
	}

	tm := flatten(td)
	gm := flatten(gd)

	rep := Report{
		TuskFiles:      len(tm),
		GoFiles:        len(gm),
		TuskPartitions: len(td.Partitions),
		GoPartitions:   len(gd.Partitions),
	}

	for path, tf := range tm {
		gf, ok := gm[path]
		if !ok {
			rep.OnlyInTusk = append(rep.OnlyInTusk, path)
			continue
		}
		if tf.Size != gf.Size {
			rep.SizeMismatch = append(rep.SizeMismatch, SizeDiff{Path: path, Tusk: tf.Size, Go: gf.Size})
		}
		tb, gb := fragBytes(tf.Fragments), fragBytes(gf.Fragments)
		if len(tf.Fragments) != len(gf.Fragments) || tb != gb {
			rep.FragMismatch = append(rep.FragMismatch, FragDiff{
				Path: path, TuskRuns: len(tf.Fragments), GoRuns: len(gf.Fragments), TuskBytes: tb, GoBytes: gb,
			})
		}
	}
	for path := range gm {
		if _, ok := tm[path]; !ok {
			rep.OnlyInGo = append(rep.OnlyInGo, path)
		}
	}

	sort.Strings(rep.OnlyInTusk)
	sort.Strings(rep.OnlyInGo)
	sort.Slice(rep.SizeMismatch, func(i, j int) bool { return rep.SizeMismatch[i].Path < rep.SizeMismatch[j].Path })
	sort.Slice(rep.FragMismatch, func(i, j int) bool { return rep.FragMismatch[i].Path < rep.FragMismatch[j].Path })

	return rep, nil
}

func flatten(d jsonDoc) map[string]jsonFile {
	m := make(map[string]jsonFile)
	for _, p := range d.Partitions {
		for _, f := range p.Files {
			m[f.Filename] = f
		}
	}
	for _, f := range d.Files { // flat (single-filesystem) layout
		m[f.Filename] = f
	}
	return m
}

func fragBytes(frags []jsonFragment) int64 {
	var n int64
	for _, f := range frags {
		n += f.End - f.Start + 1
	}
	return n
}

// Summary renders a human-readable one-screen report.
func (r Report) Summary() string {
	var b strings.Builder
	verdict := "PARITY ✓"
	if !r.Match() {
		verdict = "DIFFERENCES ✗"
	}
	fmt.Fprintf(&b, "parser-diff: %s\n", verdict)
	fmt.Fprintf(&b, "  files:      tusk=%d  go=%d\n", r.TuskFiles, r.GoFiles)
	fmt.Fprintf(&b, "  partitions: tusk=%d  go=%d\n", r.TuskPartitions, r.GoPartitions)
	fmt.Fprintf(&b, "  only in tusk: %d   only in go: %d   size mismatch: %d   fragment mismatch: %d\n",
		len(r.OnlyInTusk), len(r.OnlyInGo), len(r.SizeMismatch), len(r.FragMismatch))

	list := func(title string, items []string) {
		if len(items) == 0 {
			return
		}
		fmt.Fprintf(&b, "\n%s (%d):\n", title, len(items))
		for i, p := range items {
			if i >= 50 {
				fmt.Fprintf(&b, "  … and %d more\n", len(items)-50)
				break
			}
			fmt.Fprintf(&b, "  %s\n", p)
		}
	}
	list("only in tusk", r.OnlyInTusk)
	list("only in go", r.OnlyInGo)

	if len(r.SizeMismatch) > 0 {
		fmt.Fprintf(&b, "\nsize mismatches (%d):\n", len(r.SizeMismatch))
		for i, s := range r.SizeMismatch {
			if i >= 50 {
				fmt.Fprintf(&b, "  … and %d more\n", len(r.SizeMismatch)-50)
				break
			}
			fmt.Fprintf(&b, "  %s  tusk=%d go=%d\n", s.Path, s.Tusk, s.Go)
		}
	}
	if len(r.FragMismatch) > 0 {
		fmt.Fprintf(&b, "\nfragment mismatches (%d):\n", len(r.FragMismatch))
		for i, f := range r.FragMismatch {
			if i >= 50 {
				fmt.Fprintf(&b, "  … and %d more\n", len(r.FragMismatch)-50)
				break
			}
			fmt.Fprintf(&b, "  %s  runs tusk=%d go=%d  bytes tusk=%d go=%d\n", f.Path, f.TuskRuns, f.GoRuns, f.TuskBytes, f.GoBytes)
		}
	}
	return b.String()
}

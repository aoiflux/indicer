package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
)

// fileKey identifies a file across two manifests of the same image. Inode is
// left out: adapters may renumber synthetic IDs between versions.
type fileKey struct {
	PartOffset int64
	Path       string
	Deleted    bool
}

type manifestIndex struct {
	image      imageLine
	partitions map[int64]partitionLine
	files      map[fileKey][]fileLine
	summary    summaryLine
}

func readManifest(path string) (*manifestIndex, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	idx := &manifestIndex{partitions: map[int64]partitionLine{}, files: map[fileKey][]fileLine{}}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	for line := 1; sc.Scan(); line++ {
		var head struct {
			T string `json:"t"`
		}
		b := sc.Bytes()
		if err := json.Unmarshal(b, &head); err != nil {
			return nil, fmt.Errorf("%s:%d: %w", path, line, err)
		}
		switch head.T {
		case "image":
			err = json.Unmarshal(b, &idx.image)
		case "partition":
			var p partitionLine
			if err = json.Unmarshal(b, &p); err == nil {
				idx.partitions[p.Offset] = p
			}
		case "file":
			var fl fileLine
			if err = json.Unmarshal(b, &fl); err == nil {
				k := fileKey{fl.PartOffset, fl.Path, fl.Deleted}
				idx.files[k] = append(idx.files[k], fl)
			}
		case "summary":
			err = json.Unmarshal(b, &idx.summary)
		}
		if err != nil {
			return nil, fmt.Errorf("%s:%d: %w", path, line, err)
		}
	}
	return idx, sc.Err()
}

// diffCategories are the kinds of difference a diff reports. An allowlist
// names the categories an intended change may produce per image.
var diffCategories = []string{
	"partition_added", "partition_removed", "partition_fs_changed", "partition_error_changed",
	"file_added", "file_removed", "kind_changed", "size_changed",
	"content_changed", "content_now_known", "content_now_unknown",
	"indexed_added", "indexed_removed", "errors_changed",
}

type diffResult struct {
	counts  map[string]int
	samples map[string][]string
}

func (d *diffResult) add(cat, sample string) {
	d.counts[cat]++
	if len(d.samples[cat]) < 5 {
		d.samples[cat] = append(d.samples[cat], sample)
	}
}

// contentDigest is the file's best-known content digest: the runs digest
// when present, else the Open digest.
func contentDigest(f fileLine) string {
	if f.SHA256Runs != "" {
		return f.SHA256Runs
	}
	return f.SHA256Open
}

func errorsOf(f fileLine) string {
	return strings.Join([]string{f.StatError, f.RunsError, f.ReadError, f.OpenError}, "|")
}

func diffManifests(a, b *manifestIndex) *diffResult {
	d := &diffResult{counts: map[string]int{}, samples: map[string][]string{}}
	for off, pa := range a.partitions {
		pb, ok := b.partitions[off]
		if !ok {
			d.add("partition_removed", fmt.Sprintf("@%d %s", off, pa.FS))
			continue
		}
		if pa.FS != pb.FS {
			d.add("partition_fs_changed", fmt.Sprintf("@%d %s -> %s", off, pa.FS, pb.FS))
		}
		if (pa.OpenError + pa.WalkError) != (pb.OpenError + pb.WalkError) {
			d.add("partition_error_changed", fmt.Sprintf("@%d %q -> %q", off, pa.OpenError+pa.WalkError, pb.OpenError+pb.WalkError))
		}
	}
	for off, pb := range b.partitions {
		if _, ok := a.partitions[off]; !ok {
			d.add("partition_added", fmt.Sprintf("@%d %s", off, pb.FS))
		}
	}
	for k, fa := range a.files {
		fb, ok := b.files[k]
		if !ok {
			d.add("file_removed", fmt.Sprintf("@%d %s", k.PartOffset, k.Path))
			continue
		}
		// Duplicate keys (rare: two entries with one path) compare pairwise.
		for i := 0; i < len(fa) && i < len(fb); i++ {
			x, y := fa[i], fb[i]
			s := fmt.Sprintf("@%d %s", k.PartOffset, k.Path)
			if x.Kind != y.Kind {
				d.add("kind_changed", s+" "+x.Kind+" -> "+y.Kind)
			}
			if x.Size != y.Size {
				d.add("size_changed", fmt.Sprintf("%s %d -> %d", s, x.Size, y.Size))
			}
			cx, cy := contentDigest(x), contentDigest(y)
			switch {
			case cx == "" && cy != "":
				d.add("content_now_known", s)
			case cx != "" && cy == "":
				d.add("content_now_unknown", s)
			case cx != cy:
				d.add("content_changed", s)
			}
			if !x.DuesIndexed && y.DuesIndexed {
				d.add("indexed_added", s)
			}
			if x.DuesIndexed && !y.DuesIndexed {
				d.add("indexed_removed", s)
			}
			if errorsOf(x) != errorsOf(y) {
				d.add("errors_changed", s)
			}
		}
	}
	for k := range b.files {
		if _, ok := a.files[k]; !ok {
			d.add("file_added", fmt.Sprintf("@%d %s", k.PartOffset, k.Path))
		}
	}
	return d
}

// runDiff implements `dues-parsemanifest diff [-allow FILE] A B`. With an
// allowlist ({"<image base name>": ["category", ...]}), it exits 1 when a
// difference falls outside what is allowed for the image.
func runDiff(args []string, stdout, stderr io.Writer) int {
	var allowPath string
	if len(args) >= 2 && args[0] == "-allow" {
		allowPath, args = args[1], args[2:]
	}
	if len(args) != 2 {
		fmt.Fprintln(stderr, "usage: dues-parsemanifest diff [-allow allow.json] BEFORE.jsonl AFTER.jsonl")
		return 2
	}
	a, err := readManifest(args[0])
	if err != nil {
		fmt.Fprintln(stderr, "dues-parsemanifest:", err)
		return 2
	}
	b, err := readManifest(args[1])
	if err != nil {
		fmt.Fprintln(stderr, "dues-parsemanifest:", err)
		return 2
	}
	allowed := map[string]bool{}
	if allowPath != "" {
		raw, err := os.ReadFile(allowPath)
		if err != nil {
			fmt.Fprintln(stderr, "dues-parsemanifest:", err)
			return 2
		}
		var all map[string][]string
		if err := json.Unmarshal(raw, &all); err != nil {
			fmt.Fprintln(stderr, "dues-parsemanifest: allowlist:", err)
			return 2
		}
		valid := map[string]bool{}
		for _, c := range diffCategories {
			valid[c] = true
		}
		for img, list := range all {
			for _, c := range list {
				if !valid[c] {
					fmt.Fprintf(stderr, "dues-parsemanifest: allowlist entry %s: unknown category %q\n", img, c)
					return 2
				}
			}
		}
		for _, c := range all[baseName(b.image.Path)] {
			allowed[c] = true
		}
	}

	d := diffManifests(a, b)
	fmt.Fprintf(stdout, "%s\n", b.image.Path)
	fmt.Fprintf(stdout, "  files %d -> %d, indexed %d -> %d, errors %d -> %d, panics %d -> %d\n",
		a.summary.Files, b.summary.Files, a.summary.Indexed, b.summary.Indexed,
		a.summary.Errors, b.summary.Errors, a.summary.Panics, b.summary.Panics)
	unexpected := false
	cats := make([]string, 0, len(d.counts))
	for c := range d.counts {
		cats = append(cats, c)
	}
	sort.Strings(cats)
	for _, c := range cats {
		mark := ""
		if allowPath != "" && !allowed[c] {
			mark = "  UNEXPECTED"
			unexpected = true
		}
		fmt.Fprintf(stdout, "  %-24s %7d%s\n", c, d.counts[c], mark)
		for _, s := range d.samples[c] {
			fmt.Fprintf(stdout, "      %s\n", s)
		}
	}
	if unexpected {
		return 1
	}
	return 0
}

func baseName(p string) string {
	p = strings.ReplaceAll(p, "\\", "/")
	return p[strings.LastIndex(p, "/")+1:]
}

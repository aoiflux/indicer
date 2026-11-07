// Command mkcorpus writes the folders of the --store-jobs experiment
// (docs/BENCHMARKING.md, "Heavy evidence beside small ones"): random,
// incompressible files, the same for the same seed.
//
//   - spread/ holds the small files, a tenth of them copies of earlier ones,
//     and the heavy files at a quarter, half, three quarters and the end of
//     folder order.
//   - heavy-first/ holds the same files as hard links, the heavy ones first.
//   - manifest.json lists each file: its name in both folders, its size,
//     whether it is heavy, and which file it copies.
//
// Usage: go run ./internal/tools/mkcorpus -out DIR [flags]
//
// DIR must not exist yet. Exit status is 0 on success, 2 on a bad flag and 1
// on any other error.
package main

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"math/rand/v2"
	"os"
	"path/filepath"
)

const (
	kib = 1 << 10
	mib = 1 << 20
)

// Spec is what a corpus holds.
type Spec struct {
	Seed               uint64
	Small, Heavy       int
	SmallMin, SmallMax int64 // bytes; small sizes are log-uniform between them
	HeavyMin, HeavyMax int64 // bytes; heavy sizes are uniform between them
	Copies             float64
}

// File is one file of a corpus.
type File struct {
	Spread     string `json:"spread"`
	HeavyFirst string `json:"heavy_first"`
	Size       int64  `json:"size"`
	Heavy      bool   `json:"heavy"`
	// CopyOf is the spread name of the file this one copies; "" for none.
	CopyOf string `json:"copy_of,omitempty"`
	// content seeds the file's bytes: equal for a copy and its original.
	content uint64
}

func main() {
	os.Exit(run(os.Args[1:], os.Stderr))
}

func run(args []string, stderr io.Writer) int {
	fs := flag.NewFlagSet("mkcorpus", flag.ContinueOnError)
	fs.SetOutput(stderr)
	out := fs.String("out", "", "directory to create (must not exist)")
	var s Spec
	fs.Uint64Var(&s.Seed, "seed", 42, "seed of the sizes, the copies and the bytes")
	fs.IntVar(&s.Small, "small", 300, "number of small files")
	fs.IntVar(&s.Heavy, "heavy", 4, "number of heavy files")
	smallMin := fs.Int64("small-min-kib", 64, "smallest small file, KiB")
	smallMax := fs.Int64("small-max-mib", 64, "largest small file, MiB")
	heavyMin := fs.Int64("heavy-min-mib", 1024, "smallest heavy file, MiB")
	heavyMax := fs.Int64("heavy-max-mib", 2048, "largest heavy file, MiB")
	fs.Float64Var(&s.Copies, "copies", 0.1, "share of the small files that copy an earlier one")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	s.SmallMin, s.SmallMax = *smallMin*kib, *smallMax*mib
	s.HeavyMin, s.HeavyMax = *heavyMin*mib, *heavyMax*mib
	if *out == "" || fs.NArg() > 0 {
		fmt.Fprintln(stderr, "mkcorpus: -out DIR is required, and nothing else")
		return 2
	}
	if err := s.check(); err != nil {
		fmt.Fprintln(stderr, "mkcorpus:", err)
		return 2
	}
	files := s.Files()
	if err := write(*out, files); err != nil {
		fmt.Fprintln(stderr, "mkcorpus:", err)
		return 1
	}
	var total int64
	for _, f := range files {
		total += f.Size
	}
	fmt.Fprintf(stderr, "mkcorpus: %d files, %.2f GiB, in %s\n", len(files), float64(total)/(1<<30), *out)
	return 0
}

func (s Spec) check() error {
	switch {
	case s.Small < 0 || s.Heavy < 0 || s.Small+s.Heavy == 0:
		return errors.New("-small and -heavy must not be negative, and not both 0")
	case s.SmallMin < 1 || s.SmallMax < s.SmallMin:
		return errors.New("small sizes: need 0 < min <= max")
	case s.Heavy > 0 && (s.HeavyMin < 1 || s.HeavyMax < s.HeavyMin):
		return errors.New("heavy sizes: need 0 < min <= max")
	case s.Copies < 0 || s.Copies >= 1:
		return errors.New("-copies must be at least 0 and below 1")
	}
	return nil
}

// Files lays the corpus out: the heavy files at k/Heavy of folder order for
// k = 1..Heavy, the small ones around them.
func (s Spec) Files() []File {
	r := rand.New(rand.NewPCG(s.Seed, 0x6d6b636f72707573)) // "mkcorpus"
	total := s.Small + s.Heavy
	heavyAt := map[int]bool{}
	for k := 1; k <= s.Heavy; k++ {
		heavyAt[int(math.Round(float64(total)*float64(k)/float64(s.Heavy)))-1] = true
	}
	files := make([]File, total)
	copies := int(math.Round(float64(s.Small) * s.Copies))
	var originals []int
	small := 0
	for i := range files {
		f := &files[i]
		f.Spread = fmt.Sprintf("%04d.bin", i)
		f.content = r.Uint64()
		switch {
		case heavyAt[i]:
			f.Heavy = true
			f.Size = s.HeavyMin + r.Int64N(s.HeavyMax-s.HeavyMin+1)
		default:
			// Copies are spread over the small files after the first.
			if len(originals) > 0 && small*copies/max(s.Small, 1) < (small+1)*copies/max(s.Small, 1) {
				o := &files[originals[r.IntN(len(originals))]]
				f.Size, f.content, f.CopyOf = o.Size, o.content, o.Spread
			} else {
				lo, hi := math.Log(float64(s.SmallMin)), math.Log(float64(s.SmallMax))
				f.Size = min(s.SmallMax, max(s.SmallMin, int64(math.Exp(lo+r.Float64()*(hi-lo)))))
				originals = append(originals, i)
			}
			small++
		}
	}
	n := 0
	for pass := range 2 {
		for i := range files {
			if files[i].Heavy == (pass == 0) {
				files[i].HeavyFirst = fmt.Sprintf("%04d.bin", n)
				n++
			}
		}
	}
	return files
}

// write creates dir with the corpus in it.
func write(dir string, files []File) error {
	if _, err := os.Lstat(dir); err == nil {
		return fmt.Errorf("%s exists; give a new directory", dir)
	}
	spread, first := filepath.Join(dir, "spread"), filepath.Join(dir, "heavy-first")
	for _, d := range []string{spread, first} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}
	for _, f := range files {
		p := filepath.Join(spread, f.Spread)
		if err := writeRandom(p, f.content, f.Size); err != nil {
			return err
		}
		if err := os.Link(p, filepath.Join(first, f.HeavyFirst)); err != nil {
			return err
		}
	}
	m, err := json.MarshalIndent(files, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "manifest.json"), append(m, '\n'), 0o644)
}

// writeRandom writes size bytes from a generator seeded with seed to a new
// file at path.
func writeRandom(path string, seed uint64, size int64) (err error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, f.Close()) }()
	var key [32]byte
	binary.LittleEndian.PutUint64(key[:], seed)
	src := rand.NewChaCha8(key)
	buf := make([]byte, 4*mib)
	for left := size; left > 0; {
		b := buf[:min(int64(len(buf)), left)]
		_, _ = src.Read(b)
		if _, err := f.Write(b); err != nil {
			return err
		}
		left -= int64(len(b))
	}
	return nil
}

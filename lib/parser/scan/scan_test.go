package scan

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"sync/atomic"
	"testing"

	fixture "indicer/internal/testutil/fsfixture"
	"indicer/lib/parser/core"
	"indicer/lib/parser/image"
)

// TestWalkImageNoFilesystem exercises the full orchestration plumbing on an
// image with no partition table and no recognised filesystem: it must fall back
// to a whole-device partition, fail to mount a filesystem, and skip it — with no
// error and no records.
func TestWalkImageNoFilesystem(t *testing.T) {
	img := image.NewRaw(bytes.NewReader(make([]byte, 1<<20)), 1<<20)
	defer img.Close()

	var count int
	err := WalkImage(img, func(core.Partition, core.FSType, core.FileRecord) error {
		count++
		return nil
	})
	if err != nil {
		t.Fatalf("WalkImage: %v", err)
	}
	if count != 0 {
		t.Fatalf("records = %d, want 0 (no recognised filesystem)", count)
	}
}

// mbrDisk builds an MBR disk of total bytes whose partitions start at the
// given sectors, each 2048 sectors long, filled by fill.
func mbrDisk(total int64, starts []uint32, fill func(i int, part []byte)) []byte {
	buf := make([]byte, total)
	for i, s := range starts {
		e := buf[446+16*i : 446+16*(i+1)]
		e[4] = 0x83
		binary.LittleEndian.PutUint32(e[8:12], s)
		binary.LittleEndian.PutUint32(e[12:16], 2048)
		fill(i, buf[int64(s)*512:int64(s+2048)*512])
	}
	buf[510], buf[511] = 0x55, 0xAA
	return buf
}

// TestScanAnnouncesEveryPartition: partitions DUES cannot walk are still
// registered, with a status saying why (PAR-2, PAR-26).
func TestScanAnnouncesEveryPartition(t *testing.T) {
	buf := mbrDisk(8<<20, []uint32{2048, 4096, 6144}, func(i int, part []byte) {
		switch i {
		case 1:
			copy(part[0x10040:], "_BHRfS_M")
		case 2:
			copy(part[3:], "-FVE-FS-")
		}
	})
	img := image.NewRaw(bytes.NewReader(buf), int64(len(buf)))
	var begun []core.PartitionInfo
	var ended int
	rep, err := Scan(context.Background(), img, Visitor{
		BeginPartition: func(p core.PartitionInfo) error { begun = append(begun, p); return nil },
		EndPartition:   func(core.PartitionInfo, *core.IndexReport) error { ended++; return nil },
	}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(begun) != 3 || ended != 3 || len(rep.Partitions) != 3 {
		t.Fatalf("begun %d ended %d reported %d, want 3 each", len(begun), ended, len(rep.Partitions))
	}
	want := []struct {
		status core.PartitionStatus
		detail string
	}{{core.PartUnknown, ""}, {core.PartUnsupported, "btrfs"}, {core.PartEncrypted, "BitLocker"}}
	for i, w := range want {
		if begun[i].Status != w.status || begun[i].Detail != w.detail {
			t.Errorf("partition %d: %s %q, want %s %q", i, begun[i].Status, begun[i].Detail, w.status, w.detail)
		}
	}
}

// failAfter fails every read after the first n, like a disk that drops out.
type failAfter struct {
	core.Image
	n     int64
	reads atomic.Int64
}

var errDeviceGone = errors.New("the volume for a file has been externally altered")

func (f *failAfter) ReadAt(p []byte, off int64) (int, error) {
	if f.reads.Add(1) > f.n {
		return 0, errDeviceGone
	}
	return f.Image.ReadAt(p, off)
}

// TestScanSourceReadErrorIsFatal: a source that stops reading mid-scan ends
// the scan with a SourceReadError, not a stream of per-file errors on an
// index that looks complete (PAR-32).
func TestScanSourceReadErrorIsFatal(t *testing.T) {
	m := fixtureByName(t, "ntfs/ntfs_fsagen")
	base, err := image.Open(fixture.Open(t, m))
	if err != nil {
		t.Fatal(err)
	}
	defer base.Close()

	// Count the reads a clean scan makes, then fail half way.
	counter := &failAfter{Image: base, n: 1 << 62}
	if _, err := Scan(context.Background(), counter, Visitor{}, Options{}); err != nil {
		t.Fatal(err)
	}
	total := counter.reads.Load()

	img := &failAfter{Image: base, n: total / 2}
	var withIssues int
	var final core.PartitionStatus
	rep, err := Scan(context.Background(), img, Visitor{
		File: func(_ core.PartitionInfo, rec core.FileRecord) error {
			if len(rec.Issues) > 0 {
				withIssues++
			}
			return nil
		},
		EndPartition: func(p core.PartitionInfo, _ *core.IndexReport) error { final = p.Status; return nil },
	}, Options{})
	var serr *SourceReadError
	if !errors.As(err, &serr) || !errors.Is(err, errDeviceGone) {
		t.Fatalf("err %v, want a SourceReadError", err)
	}
	if final != core.PartReadError || rep == nil {
		t.Fatalf("partition ended %q", final)
	}
	if withIssues > 1 {
		t.Fatalf("%d records carried errors after the device went away", withIssues)
	}
}

func fixtureByName(t *testing.T, name string) fixture.Manifest {
	t.Helper()
	for _, m := range fixture.All(t) {
		if m.Name == name {
			return m
		}
	}
	t.Fatalf("no fixture %s", name)
	return fixture.Manifest{}
}

// TestScanReportsCounts: the per-partition report counts what the walk saw.
func TestScanReportsCounts(t *testing.T) {
	m := fixtureByName(t, "fat/fat32_fsagen")
	img, err := image.Open(fixture.Open(t, m))
	if err != nil {
		t.Fatal(err)
	}
	defer img.Close()
	var files, dirs int64
	rep, err := Scan(context.Background(), img, Visitor{File: func(_ core.PartitionInfo, rec core.FileRecord) error {
		switch rec.Kind {
		case core.KindFile:
			files++
		case core.KindDir:
			dirs++
		}
		return nil
	}}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Partitions) != 1 {
		t.Fatalf("%d partitions", len(rep.Partitions))
	}
	r := rep.Partitions[0]
	if r.Partition.Status != core.PartIndexed || r.Partition.FSType != core.FSFAT {
		t.Fatalf("partition %+v", r.Partition)
	}
	if r.FilesSeen != files || r.DirsSeen != dirs || files == 0 || dirs == 0 {
		t.Fatalf("report files %d dirs %d, walk files %d dirs %d", r.FilesSeen, r.DirsSeen, files, dirs)
	}
}

// TestScanCancel: a cancelled context stops the walk with its error.
func TestScanCancel(t *testing.T) {
	m := fixtureByName(t, "ext/ext4_fsagen")
	img, err := image.Open(fixture.Open(t, m))
	if err != nil {
		t.Fatal(err)
	}
	defer img.Close()
	ctx, cancel := context.WithCancel(context.Background())
	_, err = Scan(ctx, img, Visitor{File: func(core.PartitionInfo, core.FileRecord) error {
		cancel()
		return nil
	}}, Options{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err %v", err)
	}
}

// TestScanFallsBackToWholeVolume: a FAT boot sector carries the 0x55AA
// signature, so its boot code can parse as a partition table whose entries
// hold no filesystem. The scan then tries the device as one volume.
func TestScanFallsBackToWholeVolume(t *testing.T) {
	data := rawFixture(t, fixtureByName(t, "fat/fat16"))
	// One "partition" in the boot code area, pointing at zeros past the
	// FAT structures.
	e := data[446:462]
	clear(e)
	e[4] = 0x83
	binary.LittleEndian.PutUint32(e[8:12], uint32(len(data)/512-64))
	binary.LittleEndian.PutUint32(e[12:16], 32)
	img := image.NewRaw(bytes.NewReader(data), int64(len(data)))
	var files int
	rep, err := Scan(context.Background(), img, Visitor{File: func(_ core.PartitionInfo, rec core.FileRecord) error {
		if rec.Kind == core.KindFile {
			files++
		}
		return nil
	}}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Scheme != core.SchemeMBR {
		t.Skipf("libtable did not take the boot sector for an MBR (scheme %s)", rep.Scheme)
	}
	if files == 0 || len(rep.Partitions) != 2 || rep.Partitions[1].Partition.Index != -1 {
		t.Fatalf("%d files, partitions %+v", files, rep.Partitions)
	}
}

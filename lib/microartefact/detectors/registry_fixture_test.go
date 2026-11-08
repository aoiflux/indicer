package detectors

import (
	"bytes"
	"encoding/binary"
	"os"
	"strings"
	"testing"

	"indicer/lib/microartefact/model"

	"www.velocidex.com/golang/regparser"
)

// testdata/hive.dat was written with Windows' offline registry library
// (offreg.dll), which needs no privilege: a Run key with a string, a DWORD
// and a binary value, and a chain Deep\L1\...\L508 (the library's nesting
// limit) with a Marker value at L40 and L500.
func readHive(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/hive.dat")
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func values(arts []model.Artefact, name string) []model.Artefact {
	var out []model.Artefact
	for _, a := range arts {
		if a.Attributes["value_name"] == name {
			out = append(out, a)
		}
	}
	return out
}

// TestRegistryDetectorWalksAHive: every value of a real hive is found with
// its key path and type, 500 levels down too.
func TestRegistryDetectorWalksAHive(t *testing.T) {
	t.Parallel()
	arts, err := RegistryHiveDetector{MaxResults: 1000}.Detect(model.FileRecord{Name: "hive.dat"}, readHive(t))
	if err != nil {
		t.Fatal(err)
	}
	run := values(arts, "Updater")
	if len(run) != 1 || run[0].Value != `C:\Users\Public\upd.exe` || !strings.HasSuffix(run[0].Attributes["key_path"], `Microsoft\Windows\CurrentVersion\Run`) {
		t.Fatalf("Run value: %+v", run)
	}
	for _, name := range []string{"Count", "Blob"} {
		if len(values(arts, name)) != 1 {
			t.Errorf("value %s found %d times", name, len(values(arts, name)))
		}
	}
	markers := values(arts, "Marker")
	if len(markers) != 2 {
		t.Fatalf("%d markers, want the ones at L40 and L500: %+v", len(markers), markers)
	}
	for _, m := range markers {
		depth := strings.Count(m.Attributes["key_path"], `\L`)
		if m.Value != "deep-40" && m.Value != "deep-500" || (m.Value == "deep-500") != (depth == 500) {
			t.Errorf("marker %q at depth %d (%s...)", m.Value, depth, m.Attributes["key_path"][:40])
		}
	}
}

// TestRegistryDetectorSurvivesACycle: a hive is evidence, and a key may list
// an ancestor as its subkey. The walk was recursive and unbounded (GRA-13);
// it now visits each key once. The fixture is patched so that L40's subkey
// list names Deep.
func TestRegistryDetectorSurvivesACycle(t *testing.T) {
	t.Parallel()
	hive := readHive(t)
	reg, err := regparser.NewRegistry(bytes.NewReader(hive))
	if err != nil {
		t.Fatal(err)
	}
	path := `Deep`
	deep := reg.OpenKey(path)
	for i := 1; i <= 40; i++ {
		path += `\L` + itoa(i)
	}
	l40 := reg.OpenKey(path)
	if deep == nil || l40 == nil || len(l40.Subkeys()) != 1 {
		t.Fatal("the fixture is not the hive this test expects")
	}
	// A key node is 4 bytes into its cell, and cells are addressed from the
	// first hive bin at 0x1000.
	cellOf := func(k *regparser.CM_KEY_NODE) uint32 { return uint32(k.Offset - 0x1000 - 4) }
	list := 0x1000 + int64(l40.SubKeyLists()[0])
	entry := list + 4 + 4 // cell size, then "lf"/"lh" and a count
	if got := binary.LittleEndian.Uint32(hive[entry:]); got != cellOf(l40.Subkeys()[0]) {
		t.Fatalf("L40's subkey list holds %#x, want L41's cell %#x", got, cellOf(l40.Subkeys()[0]))
	}
	binary.LittleEndian.PutUint32(hive[entry:], cellOf(deep))

	arts, err := RegistryHiveDetector{MaxResults: 100000}.Detect(model.FileRecord{Name: "hive.dat"}, hive)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(values(arts, "Marker")); n != 1 {
		t.Fatalf("the marker at L40 was reported %d times, want once", n)
	}
}

func itoa(i int) string {
	var b [8]byte
	n := len(b)
	for {
		n--
		b[n] = byte('0' + i%10)
		i /= 10
		if i == 0 {
			return string(b[n:])
		}
	}
}

package dbio_test

import (
	"testing"

	"indicer/internal/testutil/testdb"
	"indicer/lib/dbio"
	"indicer/lib/structs"

	"github.com/vmihailenco/msgpack/v5"
)

// FuzzDecodeRecord decodes record values it did not write as each kind of
// record, and reads the record helpers over what decodes: an error, never a
// panic.
func FuzzDecodeRecord(f *testing.F) {
	testdb.EnsureCodecs(f)
	ev := structs.NewEvidenceFile("case.img", 0, 4096, map[string]structs.InternalOffset{"cA==": {Start: 0, End: 4095}}, "raw", "aGFzaA==")
	ix := structs.NewIndexedFile("YQ==|||/a.txt", 0, 10, "txt", false, "aGFzaA==")
	ix.Path = "/a.txt"
	for _, v := range []any{ev, ix, structs.NewPartitionFile("p", 0, 4096, nil, "aA==")} {
		b, err := msgpack.Marshal(v)
		if err != nil {
			f.Fatal(err)
		}
		f.Add(b)
	}
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, data []byte) {
		var e structs.EvidenceFile
		_ = dbio.DecodeRecord(data, &e)
		var p structs.PartitionFile
		_ = dbio.DecodeRecord(data, &p)
		var i structs.IndexedFile
		if dbio.DecodeRecord(data, &i) == nil {
			_ = dbio.RecordPath(i)
			_, _ = dbio.RecordEvidenceID(i)
			_, _ = dbio.RecordPartitionID(i)
		}
	})
}

package dbopen

import (
	"bytes"
	"context"
	"crypto/aes"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/dgraph-io/badger/v4"
	"github.com/dgraph-io/badger/v4/pb"
	"google.golang.org/protobuf/proto"

	"indicer/internal/testutil"
	"indicer/lib/cnst"
	"indicer/lib/dbmeta"
	"indicer/lib/zstdx"
)

func TestMain(m *testing.M) {
	os.Exit(testutil.RunChildScenarios(m, map[string]func(args []string) int{
		"open-read-only": childOpenReadOnly,
	}))
}

const rotationPassword = "pw"

// childOpenReadOnly opens the database at the data root args[0] read-only
// and reads every value, as a read command does.
func childOpenReadOnly(args []string) int {
	var err error
	if cnst.ENCODER, err = zstdx.Acquire("default", runtime.GOMAXPROCS(0)); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	h, err := Open(context.Background(), Request{Path: args[0], Mode: ReadOnly, Creds: dbmeta.Password(rotationPassword)})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	n := 0
	err = h.DB.View(func(txn *badger.Txn) error {
		it := txn.NewIterator(badger.DefaultIteratorOptions)
		defer it.Close()
		for it.Rewind(); it.Valid(); it.Next() {
			if _, err := it.Item().ValueCopy(nil); err != nil {
				return err
			}
			n++
		}
		return nil
	})
	if cerr := h.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	fmt.Printf("read %d values\n", n)
	return 0
}

// TestReadOnlyOpenOfAnOldEncryptedDatabase (CFG-14): badger replaces an
// encrypted database's data key once the newest is older than the rotation
// period (7 days in DUES), and writes the new key to KEYREGISTRY. A read-only
// open cannot write it, and badger then ends the process (log.Fatalf "err:
// invalid argument"), so every read command on an encrypted database not
// written for a week died, and a .duesarc extraction killed its FFI host.
func TestReadOnlyOpenOfAnOldEncryptedDatabase(t *testing.T) {
	root := setup(t)
	h := open(t, Request{Path: root, Mode: ReadWrite, Creds: dbmeta.Password(rotationPassword)})
	for i := range 8 {
		storeChunk(t, h.DB, bytes.Repeat([]byte{byte(i)}, 4096))
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	registry := filepath.Join(root, cnst.KVDBDIR, "KEYREGISTRY")
	ageKeyRegistry(t, registry, 30*24*time.Hour)
	aged, err := os.ReadFile(registry)
	if err != nil {
		t.Fatal(err)
	}

	c := testutil.StartChild(t, "open-read-only", root)
	if code := c.Wait(t, 2*time.Minute); code != 0 || !strings.Contains(c.Output(), "read ") {
		t.Fatalf("read-only open of a database whose data key is 30 days old: exit %d\n%s", code, c.Output())
	}
	if now, err := os.ReadFile(registry); err != nil || !bytes.Equal(now, aged) {
		t.Fatalf("a read-only open changed KEYREGISTRY (%v)", err)
	}

	// A writable open still replaces the old key, and the database stays
	// readable.
	h = open(t, Request{Path: root, Mode: ReadWrite, Creds: dbmeta.Password(rotationPassword)})
	storeChunk(t, h.DB, []byte("after rotation"))
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	if now, err := os.ReadFile(registry); err != nil || len(now) <= len(aged) {
		t.Fatalf("a writable open did not add a data key (%d bytes, was %d; %v)", len(now), len(aged), err)
	}
	ro := open(t, Request{Path: root, Mode: ReadOnly, Creds: dbmeta.Password(rotationPassword)})
	if err := ro.Close(); err != nil {
		t.Fatal(err)
	}
}

// ageKeyRegistry moves every data key's creation time in a badger
// KEYREGISTRY back by age, as if the database had last been written then.
// The file is an IV and the encrypted sanity text, then for each data key its
// length, CRC-32C and protobuf (badger key_registry.go).
func ageKeyRegistry(t *testing.T, path string, age time.Duration) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	head := aes.BlockSize + len("Hello Badger")
	if len(data) < head {
		t.Fatalf("KEYREGISTRY is %d bytes", len(data))
	}
	out := append([]byte(nil), data[:head]...)
	castagnoli := crc32.MakeTable(crc32.Castagnoli)
	keys := 0
	for rest := data[head:]; len(rest) > 0; keys++ {
		if len(rest) < 8 || int(binary.BigEndian.Uint32(rest[:4])) > len(rest)-8 {
			t.Fatal("KEYREGISTRY is truncated")
		}
		n := int(binary.BigEndian.Uint32(rest[:4]))
		var dk pb.DataKey
		if err := proto.Unmarshal(rest[8:8+n], &dk); err != nil {
			t.Fatal(err)
		}
		dk.CreatedAt -= int64(age / time.Second)
		rec, err := proto.Marshal(&dk)
		if err != nil {
			t.Fatal(err)
		}
		var lenCRC [8]byte
		binary.BigEndian.PutUint32(lenCRC[:4], uint32(len(rec)))
		binary.BigEndian.PutUint32(lenCRC[4:], crc32.Checksum(rec, castagnoli))
		out = append(append(out, lenCRC[:]...), rec...)
		rest = rest[8+n:]
	}
	if keys == 0 {
		t.Fatal("KEYREGISTRY holds no data key")
	}
	if err := os.WriteFile(path, out, 0o644); err != nil {
		t.Fatal(err)
	}
}

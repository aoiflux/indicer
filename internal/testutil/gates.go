package testutil

import (
	"flag"
	"os"
	"runtime"
	"testing"
)

// DirFlag is a go test flag naming a directory a test needs that a machine
// may not have, such as a special mount or the image corpus. A test whose
// directory was not given skips.
type DirFlag struct {
	name string
	dir  *string
}

// NewDirFlag defines the flag -name. Pass it after -args and name the
// packages, since a test binary refuses a flag it does not define:
//
//	go test ./internal/e2e -args -dues.enospc-dir=/mnt/dues-enospc
func NewDirFlag(name, usage string) *DirFlag {
	return &DirFlag{name: name, dir: flag.String(name, "", usage)}
}

// Require returns the directory, or skips the test when the flag was not
// given. A directory that does not exist fails the test.
func (f *DirFlag) Require(tb testing.TB) string {
	tb.Helper()
	dir := *f.dir
	if dir == "" {
		tb.Skipf("pass -args -%s=DIR to run it", f.name)
	}
	switch info, err := os.Stat(dir); {
	case err != nil:
		tb.Fatalf("-%s=%s: %v", f.name, dir, err)
	case !info.IsDir():
		tb.Fatalf("-%s=%s is not a directory", f.name, dir)
	}
	return dir
}

// The special mounts the filesystem-failure tests need, which
// scripts/wsl/mounts.sh sets up; `mounts.sh args` prints these flags. They
// live here so one -args list serves every package that uses them.
var (
	// ENOSPCDir is a 16 MB filesystem: writes fill it, so a test sees a
	// real disk-full error.
	ENOSPCDir = NewDirFlag("dues.enospc-dir", "a 16 MB filesystem that writes fill (scripts/wsl/mounts.sh)")
	// LowSpaceDir has room for a database and DUES's 128 MiB free-space
	// reserve, so a store fills it part way.
	LowSpaceDir = NewDirFlag("dues.lowspace-dir", "a 384 MB filesystem that a store fills part way")
	// ReadOnlySrc is a writable directory whose read-only view is
	// ReadOnlyDB, so a test can build a database and read it as from
	// write-protected media.
	ReadOnlySrc = NewDirFlag("dues.ro-src", "a writable directory whose read-only view is -dues.ro-db")
	// ReadOnlyDB is the read-only view of ReadOnlySrc.
	ReadOnlyDB = NewDirFlag("dues.ro-db", "a read-only view of -dues.ro-src")
)

// RequireLinuxNonRoot skips unless the test runs on Linux as a non-root user,
// which permission tests need: root bypasses file modes.
func RequireLinuxNonRoot(tb testing.TB) {
	tb.Helper()
	if runtime.GOOS != "linux" {
		tb.Skip("needs Linux")
	}
	if os.Geteuid() == 0 {
		tb.Skip("needs a non-root user; root ignores file permissions")
	}
}

// LongTest skips scale and soak tests that take minutes unless the test
// binary was built with -tags long, and always under -short.
func LongTest(tb testing.TB) {
	tb.Helper()
	if testing.Short() {
		tb.Skip("long test skipped under -short")
	}
	if !longTests {
		tb.Skip("long test; run with -tags long")
	}
}

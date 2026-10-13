package testutil

import (
	"os"
	"runtime"
	"testing"
)

// RequireEnvDir returns the directory named by the environment variable, or
// skips the test when it is unset. Harness scripts set these variables for
// tests that need a special mount (DUES_TEST_ENOSPC_DIR, DUES_TEST_RO_DIR) or
// the real-image corpus (DUES_TEST_CORPUS).
func RequireEnvDir(tb testing.TB, name string) string {
	tb.Helper()
	dir := os.Getenv(name)
	if dir == "" {
		tb.Skipf("%s is not set", name)
	}
	info, err := os.Stat(dir)
	if err != nil {
		tb.Fatalf("%s=%s: %v", name, dir, err)
	}
	if !info.IsDir() {
		tb.Fatalf("%s=%s is not a directory", name, dir)
	}
	return dir
}

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

// LongTest skips under -short and unless DUES_LONG_TESTS=1, for scale and
// soak tests that take minutes.
func LongTest(tb testing.TB) {
	tb.Helper()
	if testing.Short() {
		tb.Skip("long test skipped under -short")
	}
	if os.Getenv("DUES_LONG_TESTS") != "1" {
		tb.Skip("set DUES_LONG_TESTS=1 to run long tests")
	}
}

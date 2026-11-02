package main

import (
	"os"
	"testing"
)

// TestMain keeps the developer's own config file out of the tests: the
// default config file is one that does not exist. Tests that want a
// file pass --config.
func TestMain(m *testing.M) {
	defaultConfigPath = func() (string, error) { return "testdata/no-such-config.json", nil }
	os.Exit(m.Run())
}

package main

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"indicer/internal/lifecycle"
	"indicer/internal/testutil/testdb"
	"indicer/lib/cnst"
	"indicer/lib/dbopen"
)

// configure parses args the way run does and applies the runtime config.
func configure(t *testing.T, args ...string) (pipelineTuning, error) {
	t.Helper()
	// No test reads the chunk size of a database it did not make: with no
	// --dbpath, that would be the account's own.
	prev := databaseChunkKiB
	t.Cleanup(func() { databaseChunkKiB = prev })
	databaseChunkKiB = func(string) int { return 0 }
	return configureOn(t, args...)
}

// configureOn is configure, reading the chunk size of the database at
// --dbpath, as run does.
func configureOn(t *testing.T, args ...string) (pipelineTuning, error) {
	t.Helper()
	testdb.Isolate(t)
	cliDef := newCLIDefinition()
	parsed, err := cliDef.app.Parse(args)
	if err != nil {
		t.Fatalf("parse %q: %v", args, err)
	}
	return applyRuntimeConfig(parsed, cliDef, cmdLineSources(t, cliDef, args))
}

func TestConfigPresets(t *testing.T) {
	cases := []struct {
		args         []string
		preset       string
		low, quick   bool
		compressFlag string
	}{
		{[]string{"list"}, "", false, false, "best"},
		{[]string{"--preset", "quick", "list"}, cnst.PresetQuick, false, true, "fast"},
		{[]string{"--quick-mode", "list"}, cnst.PresetQuick, false, true, "fast"},
		{[]string{"-Q", "list"}, cnst.PresetQuick, false, true, "fast"},
		{[]string{"--preset", "low-resource", "list"}, cnst.PresetLowResource, true, false, "default"},
		{[]string{"--preset", "performance", "list"}, cnst.PresetPerformance, false, false, "best"},
		// Explicit flags win over the preset's defaults.
		{[]string{"--preset", "quick", "-z", "best", "list"}, cnst.PresetQuick, false, true, "best"},
		{[]string{"--no-quick-mode", "list"}, "", false, false, "best"},
		{[]string{"--preset", "performance", "--no-quick-mode", "list"}, cnst.PresetPerformance, false, false, "best"},
		// Regressions: whether a flag was set comes from the parser, not from
		// scanning argv. A negated flag counts as set, and an operand that
		// looks like a flag does not.
		{[]string{"--preset", "quick", "--no-quick", "list"}, cnst.PresetQuick, false, false, "fast"},
		{[]string{"--preset", "quick", "search", "--", "-z"}, cnst.PresetQuick, false, true, "fast"},
	}
	for _, c := range cases {
		t.Run(strings.Join(c.args, " "), func(t *testing.T) {
			tuning, err := configure(t, c.args...)
			if err != nil {
				t.Fatal(err)
			}
			if tuning.activePreset != c.preset || cnst.MEMOPT != c.low || cnst.QUICKOPT != c.quick || cnst.CompressLevel != c.compressFlag {
				t.Fatalf("preset %q low %v quick %v level %q; want %q %v %v %q",
					tuning.activePreset, cnst.MEMOPT, cnst.QUICKOPT, cnst.CompressLevel,
					c.preset, c.low, c.quick, c.compressFlag)
			}
		})
	}
}

func TestConfigRejectsBadFlags(t *testing.T) {
	cases := []struct {
		args []string
		flag string
	}{
		{[]string{"-c", "0", "list"}, cnst.FlagChonkSize},
		{[]string{"-c", "130", "list"}, cnst.FlagChonkSize},
		{[]string{"-z", "ultra", "list"}, cnst.FlagCompressLevel},
		{[]string{"--preset", "turbo", "list"}, cnst.FlagPreset},
		{[]string{"--preset", "performance", "--quick-mode", "list"}, cnst.FlagPreset},
		{[]string{"list", "--status", "done"}, cnst.FlagListStatus},
		{[]string{"search", "--rank-alpha", "NaN", "q"}, cnst.FlagRankAlpha},
		{[]string{"near", "in", "--top-k", "5000", "h"}, cnst.FlagTopK},
		{[]string{"store", "--store-workers", "100000", "x"}, cnst.FlagStoreWorkers},
		{[]string{"store", "--hash-algo", "md5", "x"}, cnst.FlagHashAlgo},
	}
	for _, c := range cases {
		t.Run(strings.Join(c.args, " "), func(t *testing.T) {
			_, err := configure(t, c.args...)
			if err == nil {
				t.Fatal("accepted")
			}
			if code := lifecycle.ExitCode(err); code != lifecycle.ExitUsage {
				t.Errorf("exit code %d, want %d: %v", code, lifecycle.ExitUsage, err)
			}
			if !strings.Contains(err.Error(), "--"+c.flag) {
				t.Errorf("error does not name --%s: %v", c.flag, err)
			}
		})
	}
}

// TestConfigHierarchicalRetired: -i no longer selects a storage mode (D9).
func TestConfigHierarchicalRetired(t *testing.T) {
	tuning, err := configure(t, "-i", "list")
	if err != nil {
		t.Fatal(err)
	}
	if cnst.CONTAINERMODE {
		t.Fatal("-i enabled container mode")
	}
	for _, issue := range tuning.issues {
		if issue.Flag == cnst.FlagHierarchicalIndex {
			return
		}
	}
	t.Fatal("-i gave no notice")
}

// TestConfigAsyncFIODeprecated covers the retired --async-fio (STO-4). It
// still parses, so existing scripts keep working, but it changes nothing and
// says so.
func TestConfigAsyncFIODeprecated(t *testing.T) {
	warned := func(tuning pipelineTuning) bool {
		for _, issue := range tuning.issues {
			if issue.Flag == cnst.FlagStoreAsyncFileWrite {
				return true
			}
		}
		return false
	}
	for _, flag := range []string{"--async-fio", "-A"} {
		tuning, err := configure(t, "store", flag, "x")
		if err != nil {
			t.Fatalf("%s: %v", flag, err)
		}
		if !warned(tuning) {
			t.Fatalf("%s gave no deprecation warning", flag)
		}
	}
	tuning, err := configure(t, "store", "x")
	if err != nil {
		t.Fatal(err)
	}
	if warned(tuning) {
		t.Fatal("deprecation warning without --async-fio")
	}
}

// TestConfigIndexFlagsRetired: --hocho-mode and --sync are accepted with a
// warning and change nothing (W6: partition and indexed-file hashes are
// plain hashes, and the index pass always runs beside storage).
func TestConfigIndexFlagsRetired(t *testing.T) {
	for _, tc := range []struct {
		args []string
		flag string
	}{
		{[]string{"store", "--hocho-mode", "reuse", "x"}, cnst.FlagHochoMode},
		{[]string{"store", "--hash-strategy", "hocho", "--hocho-mode", "postdedup", "x"}, cnst.FlagHochoMode},
		{[]string{"store", "--sync", "x"}, cnst.FlagSyncIndex},
	} {
		tuning, err := configure(t, tc.args...)
		if err != nil {
			t.Fatalf("%v: %v", tc.args, err)
		}
		warned := false
		for _, issue := range tuning.issues {
			warned = warned || issue.Flag == tc.flag
		}
		if !warned {
			t.Errorf("%v gave no retirement warning", tc.args)
		}
	}
}

func TestConfigStoreTuningOverrides(t *testing.T) {
	tuning, err := configure(t, "store", "-w", "3", "-W", "9", "x")
	if err != nil {
		t.Fatal(err)
	}
	if tuning.storeWorkers != 3 || tuning.storeQueue != 9 {
		t.Fatalf("tuning %d/%d, want 3/9", tuning.storeWorkers, tuning.storeQueue)
	}
	if w, q := cnst.GetStorePipelineTuning(cnst.CONTAINERMODE); w != 3 || q != 9 {
		t.Fatalf("pipeline sees %d/%d, want 3/9", w, q)
	}
}

// TestConfigFitsTheDatabasesChunks: a store's workers and queue are fitted to
// the chunks of the database it opens, not to --chonksize's default: 8
// workers and a queue of 4096 hold 1 GiB of 256 KiB chunks, and 256 GiB of
// this database's 64 MiB ones.
func TestConfigFitsTheDatabasesChunks(t *testing.T) {
	testdb.Isolate(t)
	root := filepath.Join(t.TempDir(), "db")
	h, err := dbopen.Open(t.Context(), dbopen.Request{Path: root, Mode: dbopen.ReadWrite, Layout: dbopen.Requested{ChunkKiB: 65536}})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	_, err = configureOn(t, "-d", root, "store", "-w", "8", "-W", "4096", "x")
	var usage *lifecycle.UsageError
	if !errors.As(err, &usage) || !strings.Contains(err.Error(), "the database's 65536 KiB chunks") {
		t.Fatalf("store -w 8 -W 4096 into a database of 64 MiB chunks: %v; want a usage error naming its chunks", err)
	}
}

// cmdLineSources is where run finds each flag's value when no config file
// is read: the command line, else the default.
func cmdLineSources(t testing.TB, cliDef cliDefinition, args []string) sources {
	t.Helper()
	set, err := flagsSetByUser(cliDef.app, args)
	if err != nil {
		t.Fatal(err)
	}
	return sources{set: set, filled: map[string]bool{}}
}

//go:build faultinject

package faultinject

import (
	"errors"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"indicer/internal/testutil"
)

func TestMain(m *testing.M) {
	os.Exit(testutil.RunChildScenarios(m, map[string]func() int{
		// hit reaches the self-test point once and exits 0 if nothing fired.
		"hit": func() int {
			sig := make(chan os.Signal, 1)
			signal.Notify(sig, os.Interrupt)
			if err := Hit(SelfTest); err != nil {
				return 1
			}
			select {
			case <-sig:
				return 130
			case <-time.After(200 * time.Millisecond):
				return 0
			}
		},
	}))
}

func withSpec(t *testing.T, spec string) {
	t.Helper()
	restore, err := Set(spec)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(restore)
}

func TestConfigureRejectsBadSpecAndKeepsRules(t *testing.T) {
	withSpec(t, SelfTest+"=error")
	for _, spec := range []string{"nope", "no.such.point=error", SelfTest + "=exit", SelfTest + "=error@0"} {
		if err := Configure(spec, ""); err == nil {
			t.Errorf("Configure(%q) accepted a bad spec", spec)
		}
	}
	if err := Hit(SelfTest); !errors.Is(err, ErrInjected) {
		t.Fatalf("after bad specs the earlier rule is gone: %v", err)
	}
}

func TestConfigureReportsFirstHits(t *testing.T) {
	withSpec(t, "")
	notify := filepath.Join(t.TempDir(), "points.log")
	if err := Configure(SelfTest+"=sleep:1ms", notify); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if err := Hit(SelfTest); err != nil {
			t.Fatal(err)
		}
	}
	b, err := os.ReadFile(notify)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(b); got != SelfTest+"\n" {
		t.Fatalf("notification file %q, want the point once", got)
	}
	if Hits(SelfTest) != 3 {
		t.Fatalf("Hits = %d, want 3", Hits(SelfTest))
	}
}

func TestSetRestoresAndCountsAgain(t *testing.T) {
	withSpec(t, SelfTest+"=error@2")
	if err := Hit(SelfTest); err != nil {
		t.Fatal(err)
	}
	restore, err := Set("")
	if err != nil {
		t.Fatal(err)
	}
	if err := Hit(SelfTest); err != nil {
		t.Fatalf("an empty spec injected %v", err)
	}
	restore()
	if Hits(SelfTest) != 0 {
		t.Fatalf("Hits = %d after restore, want 0", Hits(SelfTest))
	}
	if err := Hit(SelfTest); err != nil {
		t.Fatalf("first hit after restore: %v", err)
	}
	if err := Hit(SelfTest); !errors.Is(err, ErrInjected) {
		t.Fatalf("second hit after restore: %v, want the restored rule", err)
	}
}

func TestHitError(t *testing.T) {
	withSpec(t, SelfTest+"=error@2")
	if err := Hit(SelfTest); err != nil {
		t.Fatalf("first hit: %v", err)
	}
	if err := Hit(SelfTest); !errors.Is(err, ErrInjected) {
		t.Fatalf("second hit: %v, want ErrInjected", err)
	}
	if err := Hit(SelfTest); err != nil {
		t.Fatalf("third hit: %v", err)
	}
	if Hits(SelfTest) != 3 {
		t.Fatalf("Hits = %d", Hits(SelfTest))
	}
}

func TestHitOSErrors(t *testing.T) {
	for _, action := range []string{"enospc", "eio", "eacces"} {
		withSpec(t, SelfTest+"="+action)
		if err := Hit(SelfTest); err == nil {
			t.Errorf("%s: no error", action)
		}
	}
	withSpec(t, SelfTest+"=enospc")
	if err := Hit(SelfTest); !errors.Is(err, testutil.ErrNoSpace()) {
		t.Errorf("enospc is not the platform disk-full error: %v", err)
	}
}

func TestHitPanic(t *testing.T) {
	withSpec(t, SelfTest+"=panic")
	defer func() {
		if recover() == nil {
			t.Fatal("no panic")
		}
	}()
	_ = Hit(SelfTest)
}

func TestHitUnknownPointPanics(t *testing.T) {
	withSpec(t, "")
	defer func() {
		if recover() == nil {
			t.Fatal("an unregistered point must panic")
		}
	}()
	_ = Hit("not.registered")
}

func TestHitExitInChild(t *testing.T) {
	c := testutil.StartChild(t, "hit", "DUES_FAULTS="+SelfTest+"=exit:42")
	if code := c.Wait(t, 30*time.Second); code != 42 {
		t.Fatalf("exit code %d, want 42\n%s", code, c.Output())
	}
}

func TestHitBlockNotifiesAndKill(t *testing.T) {
	c := testutil.StartChild(t, "hit", "DUES_FAULTS="+SelfTest+"=block")
	c.WaitForPoint(t, SelfTest+"#blocked", 30*time.Second)
	c.Kill(t)
}

func TestHitSignalInChild(t *testing.T) {
	if runtime.GOOS == "windows" && os.Getenv("DUES_TEST_CONSOLE") == "" {
		// Ctrl-Break needs the child attached to a console; CI runners
		// without one deliver nothing.
		t.Skip("set DUES_TEST_CONSOLE=1 when running in a console")
	}
	c := testutil.StartChild(t, "hit", "DUES_FAULTS="+SelfTest+"=signal:INT")
	if code := c.Wait(t, 30*time.Second); code != 130 {
		t.Fatalf("exit code %d, want 130\n%s", code, c.Output())
	}
}

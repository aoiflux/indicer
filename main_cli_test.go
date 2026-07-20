package main

import (
	"io"
	"os"
	"strings"
	"testing"

	"github.com/fatih/color"
)

func strPtr(v string) *string { return &v }
func boolPtr(v bool) *bool    { return &v }

func TestResolvePreset_FromPresetFlag(t *testing.T) {
	flags := cliFlags{
		preset:          strPtr("quick"),
		quickMode:       boolPtr(false),
		performanceMode: boolPtr(false),
		lowResourceMode: boolPtr(false),
	}

	preset, err := resolvePreset([]string{"--preset", "quick"}, flags)
	if err != nil {
		t.Fatalf("resolvePreset returned error: %v", err)
	}
	if preset != "quick" {
		t.Fatalf("expected quick preset, got %q", preset)
	}
}

func TestResolvePreset_FromConvenienceFlag(t *testing.T) {
	flags := cliFlags{
		preset:          strPtr(""),
		quickMode:       boolPtr(true),
		performanceMode: boolPtr(false),
		lowResourceMode: boolPtr(false),
	}

	preset, err := resolvePreset([]string{"--quick-mode"}, flags)
	if err != nil {
		t.Fatalf("resolvePreset returned error: %v", err)
	}
	if preset != "quick" {
		t.Fatalf("expected quick preset, got %q", preset)
	}
}

func TestResolvePreset_ConflictingPresetsError(t *testing.T) {
	flags := cliFlags{
		preset:          strPtr("performance"),
		quickMode:       boolPtr(true),
		performanceMode: boolPtr(false),
		lowResourceMode: boolPtr(false),
	}

	_, err := resolvePreset([]string{"--preset", "performance", "--quick-mode"}, flags)
	if err == nil {
		t.Fatal("expected conflict error, got nil")
	}
}

func TestApplyPresetDefaults_ExplicitFlagsOverridePreset(t *testing.T) {
	memOpt, quickOpt, compressLevel, err := applyPresetDefaults(
		"quick",
		[]string{"-z", "best", "-q"},
		false,
		true,
		"best",
	)
	if err != nil {
		t.Fatalf("applyPresetDefaults returned error: %v", err)
	}
	if !quickOpt {
		t.Fatal("expected quickOpt to remain true due to explicit -q")
	}
	if compressLevel != "best" {
		t.Fatalf("expected explicit compress level to win (best), got %q", compressLevel)
	}
	if memOpt {
		t.Fatalf("expected memOpt false for quick preset without explicit low flag, got %v", memOpt)
	}
}

func TestApplyPresetDefaults_LowResourcePreset(t *testing.T) {
	memOpt, quickOpt, compressLevel, err := applyPresetDefaults(
		"low-resource",
		[]string{},
		false,
		true,
		"best",
	)
	if err != nil {
		t.Fatalf("applyPresetDefaults returned error: %v", err)
	}
	if !memOpt {
		t.Fatal("expected memOpt true for low-resource preset")
	}
	if quickOpt {
		t.Fatal("expected quickOpt false for low-resource preset")
	}
	if compressLevel != "default" {
		t.Fatalf("expected default compression for low-resource preset, got %q", compressLevel)
	}
}

func TestHasAnyFlagArg_LongAndShort(t *testing.T) {
	if !hasAnyFlagArg([]string{"--preset", "quick"}, "preset", 'P') {
		t.Fatal("expected long flag match")
	}
	if !hasAnyFlagArg([]string{"-P", "quick"}, "preset", 'P') {
		t.Fatal("expected short flag match")
	}
	if hasAnyFlagArg([]string{"--other"}, "preset", 'P') {
		t.Fatal("did not expect match")
	}
}

func TestPrintHelpForPath_NearInRoutesToNearInHelp(t *testing.T) {
	out := captureStdout(t, func() {
		printHelpForPath([]string{"near", "in"})
	})
	if !strings.Contains(out, "Command: near in") {
		t.Fatalf("expected near in help header, got: %s", out)
	}
}

func TestPrintHelpForPath_MicroExtractRoutesToMicroExtractHelp(t *testing.T) {
	out := captureStdout(t, func() {
		printHelpForPath([]string{"micro", "extract"})
	})
	if !strings.Contains(out, "Command: micro extract") {
		t.Fatalf("expected micro extract help header, got: %s", out)
	}
}

func TestPrintHelpForPath_MicroListRoutesToMicroListHelp(t *testing.T) {
	out := captureStdout(t, func() {
		printHelpForPath([]string{"micro", "list"})
	})
	if !strings.Contains(out, "Command: micro list") {
		t.Fatalf("expected micro list help header, got: %s", out)
	}
}

func TestPrintHelpForPath_UnknownFallsBackToRootHelp(t *testing.T) {
	out := captureStdout(t, func() {
		printHelpForPath([]string{"does-not-exist"})
	})
	if !strings.Contains(out, "DUES - Deduplicated Unified Evidence Store") {
		t.Fatalf("expected root help fallback, got: %s", out)
	}
}

func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	oldStdout := os.Stdout
	oldNoColor := color.NoColor
	color.NoColor = true

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe create failed: %v", err)
	}
	os.Stdout = w

	fn()

	_ = w.Close()
	os.Stdout = oldStdout
	color.NoColor = oldNoColor

	b, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read stdout failed: %v", err)
	}
	_ = r.Close()
	return string(b)
}

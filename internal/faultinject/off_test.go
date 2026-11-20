//go:build !faultinject

package faultinject

import "testing"

// A release build injects nothing, whatever the environment says.
func TestHitIsInertWithoutTag(t *testing.T) {
	t.Setenv("DUES_FAULTS", SelfTest+"=panic")
	if Enabled {
		t.Fatal("Enabled without the faultinject tag")
	}
	if err := Hit(SelfTest); err != nil {
		t.Fatalf("Hit returned %v", err)
	}
}

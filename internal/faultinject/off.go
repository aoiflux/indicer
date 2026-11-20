//go:build !faultinject

package faultinject

// Enabled reports whether this build can inject faults.
const Enabled = false

// Hit marks a fault point. Without the faultinject build tag it does nothing.
func Hit(string) error { return nil }

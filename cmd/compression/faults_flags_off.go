//go:build !faultinject

package main

import "flag"

// faultFlags is empty in a release build: it takes no fault options.
type faultFlags struct{}

func (*faultFlags) register(*flag.FlagSet) {}

func (faultFlags) apply() error { return nil }

//go:build !faultinject

package main

import "github.com/alecthomas/kingpin/v2"

// faultFlags is empty in a release build: it takes no fault flags.
type faultFlags struct{}

func addFaultFlags(*kingpin.Application) faultFlags { return faultFlags{} }

func (faultFlags) apply() error { return nil }

//go:build faultinject

package main

import (
	"flag"
	"fmt"

	"indicer/internal/faultinject"
)

// faultFlags are the global options a faultinject build takes: the fault
// spec and the file each point's first hit is reported to. A release build
// has neither, so passing one there is an unknown option.
type faultFlags struct {
	spec, notify string
}

func (f *faultFlags) register(fs *flag.FlagSet) {
	fs.StringVar(&f.spec, "faults", "", "inject faults: point=action[@N[+]],... (test builds only)")
	fs.StringVar(&f.notify, "faults-notify", "", "append each fault point's first hit to this file (test builds only)")
}

// apply installs the options' faults. Neither option given leaves the
// process as it was.
func (f faultFlags) apply() error {
	if f.spec == "" && f.notify == "" {
		return nil
	}
	if err := faultinject.Configure(f.spec, f.notify); err != nil {
		return fmt.Errorf("--faults: %w", err)
	}
	return nil
}

//go:build faultinject

package main

import (
	"indicer/internal/faultinject"
	"indicer/internal/lifecycle"

	"github.com/alecthomas/kingpin/v2"
)

// faultFlags are the hidden flags a faultinject build takes: the fault spec
// and the file each point's first hit is reported to. A release build has
// neither, so passing one there is an unknown flag.
type faultFlags struct {
	spec, notify *string
}

func addFaultFlags(app *kingpin.Application) faultFlags {
	return faultFlags{
		spec:   app.Flag("faults", "Inject faults: point=action[@N[+]],... (test builds only)").Hidden().String(),
		notify: app.Flag("faults-notify", "Append each fault point's first hit to this file (test builds only)").Hidden().String(),
	}
}

// apply installs the flags' faults. Neither flag given leaves the process
// as it was.
func (f faultFlags) apply() error {
	if *f.spec == "" && *f.notify == "" {
		return nil
	}
	if err := faultinject.Configure(*f.spec, *f.notify); err != nil {
		return lifecycle.Usagef("--faults: %v", err)
	}
	return nil
}

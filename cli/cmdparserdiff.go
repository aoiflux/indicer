package cli

import (
	"fmt"

	"indicer/lib/parser"
	"indicer/lib/parser/pdiff"
	"indicer/lib/parser/tskcompat"
)

// ParserDiff runs both evidence-parser backends on the image at imagePath — the
// CGo libtusk parser and the pure-Go stack — and prints a parity report. It is a
// migration validation aid (see docs/LIBTSK_REMOVAL_PLAN.md).
func ParserDiff(imagePath string) error {
	tuskJSON, hasTusk := parser.TuskAnalysis(imagePath)
	if !hasTusk {
		return fmt.Errorf("libtusk parser unavailable or failed for %q (a non-notusk CGo build is required)", imagePath)
	}

	goJSON, err := tskcompat.Analyze(imagePath)
	if err != nil {
		return fmt.Errorf("go parser failed: %w", err)
	}

	rep, err := pdiff.Compare(tuskJSON, goJSON)
	if err != nil {
		return err
	}
	fmt.Print(rep.Summary())
	return nil
}

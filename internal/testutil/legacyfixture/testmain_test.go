package legacyfixture

import (
	"fmt"
	"os"
	"testing"

	"indicer/internal/testutil"
	"indicer/internal/testutil/leakcheck"
)

func TestMain(m *testing.M) {
	code := leakcheck.Run(m)
	if err := testutil.RemoveBuilds(); err != nil {
		fmt.Fprintln(os.Stderr, "remove the test builds:", err)
	}
	os.Exit(code)
}

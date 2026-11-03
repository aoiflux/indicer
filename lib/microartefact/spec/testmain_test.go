package spec

import (
	"os"
	"testing"

	"indicer/internal/testutil/leakcheck"
)

func TestMain(m *testing.M) { os.Exit(leakcheck.Run(m)) }

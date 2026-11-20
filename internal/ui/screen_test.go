package ui

import (
	"testing"

	"indicer/internal/testutil/vtscreen"
)

// screen is the terminal tests draw on (internal/testutil/vtscreen).
type screen = vtscreen.Screen

func newScreen(t *testing.T, width int, opts ...vtscreen.Option) *screen {
	return vtscreen.New(t, width, opts...)
}

// newScreenTaskbar is a screen that also shows taskbar progress.
func newScreenTaskbar(t *testing.T, width int) *screen {
	return vtscreen.New(t, width, vtscreen.AllowTaskbar())
}

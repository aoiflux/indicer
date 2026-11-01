//go:build !safestrict

package safe

// strictDefault: release and ordinary test builds contain panics.
const strictDefault = false

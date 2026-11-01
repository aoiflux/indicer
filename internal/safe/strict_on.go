//go:build safestrict

package safe

// strictDefault: a build with the safestrict tag (the fuzz smoke) re-panics
// every recovered panic, so a contained crash is still a finding.
const strictDefault = true

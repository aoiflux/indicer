//go:build requirepty

package e2e

// requirePTY turns the pseudo-terminal tests' skip into a failure: -tags
// requirepty, which the WSL suite uses.
const requirePTY = true

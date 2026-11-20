//go:build faultinject && !winconsole

package faultinject

// winConsole reports that the tests run attached to a Windows console, which
// Ctrl-Break needs: -tags winconsole.
const winConsole = false

package main

import (
	"os"
	"testing"
)

// TestAtArgumentsAreNotExpanded is the regression for kingpin's @file
// expansion, on by default: "dues store @ev.bin" read ev.bin as more
// arguments instead of storing the file named @ev.bin.
func TestAtArgumentsAreNotExpanded(t *testing.T) {
	t.Chdir(t.TempDir())
	for _, name := range []string{"ev.bin", "case"} {
		if err := os.WriteFile(name, []byte("--plain\nnot-this\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cliDef := newCLIDefinition()
	if _, err := cliDef.app.Parse([]string{"--dbpath", "@case", "store", "@ev.bin"}); err != nil {
		t.Fatal(err)
	}
	if f := cliDef.flags; *f.evipath != "@ev.bin" || *f.dbpath != "@case" || *f.plain {
		t.Fatalf("store %q --dbpath %q --plain %v; want the arguments as given", *f.evipath, *f.dbpath, *f.plain)
	}
}

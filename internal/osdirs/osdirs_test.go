package osdirs

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

var printChild = flag.Bool("osdirs.print", false, "print the directories as JSON (TestIgnoresEnvironment's child)")

// envVars are the variables the standard library and the operating system
// conventionally consult for these directories and the account name.
var envVars = []string{
	"HOME", "USER", "LOGNAME", "TMPDIR", "XDG_CACHE_HOME", "XDG_CONFIG_HOME",
	"USERPROFILE", "USERNAME", "TMP", "TEMP", "LOCALAPPDATA", "APPDATA", "HOMEDRIVE", "HOMEPATH",
	"SystemDrive",
}

type result struct {
	Dir string `json:"dir,omitempty"`
	Err string `json:"err,omitempty"`
}

func lookups() map[string]result {
	out := map[string]result{}
	for name, f := range map[string]func() (string, error){
		"Temp": Temp, "Home": Home, "Cache": Cache, "LogDir": LogDir, "UserName": UserName,
	} {
		dir, err := f()
		r := result{Dir: dir}
		if err != nil {
			r.Err = err.Error()
		}
		out[name] = r
	}
	return out
}

// TestPrintDirs is the child of TestIgnoresEnvironment.
func TestPrintDirs(t *testing.T) {
	t.Parallel()
	if !*printChild {
		t.Skip("run by TestIgnoresEnvironment")
	}
	if err := json.NewEncoder(os.Stdout).Encode(lookups()); err != nil {
		t.Fatal(err)
	}
}

// TestIgnoresEnvironment sets every variable the conventional lookups read
// to a junk directory and checks that nothing changes, in this process and
// in a child started with the junk environment.
func TestIgnoresEnvironment(t *testing.T) {
	want := lookups()
	junk := "/nonexistent/junk"
	if runtime.GOOS == "windows" {
		junk = `Q:\nonexistent\junk`
	}
	for _, v := range envVars {
		t.Setenv(v, junk+"-"+v)
	}
	if got := lookups(); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("in process with a junk environment:\n got %v\nwant %v", got, want)
	}

	// Started with the junk environment, the child cannot see results
	// cached before the variables changed.
	cmd := exec.Command(os.Args[0], "-test.run=^TestPrintDirs$", "-osdirs.print")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("child: %v\n%s", err, out)
	}
	line, _, _ := strings.Cut(string(out), "\n")
	var got map[string]result
	if err := json.Unmarshal([]byte(line), &got); err != nil {
		t.Fatalf("child output %q: %v", out, err)
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("child with a junk environment:\n got %v\nwant %v", got, want)
	}
	for name, r := range got {
		if strings.Contains(r.Dir, "junk") {
			t.Errorf("%s = %q comes from the environment", name, r.Dir)
		}
	}
}

func TestDirsAreAbsolute(t *testing.T) {
	t.Parallel()
	for name, r := range lookups() {
		if name == "UserName" {
			continue
		}
		if r.Err != "" {
			// A uid with no account entry (a container run as an
			// arbitrary uid) has no home; the error must say why.
			if name == "Temp" && runtime.GOOS != "windows" {
				t.Errorf("Temp: %s", r.Err)
			}
			t.Logf("%s: %s", name, r.Err)
			continue
		}
		if !filepath.IsAbs(r.Dir) {
			t.Errorf("%s = %q, not absolute", name, r.Dir)
		}
	}
}

func TestLayout(t *testing.T) {
	t.Parallel()
	cache, err := Cache()
	if err != nil {
		t.Skipf("no cache directory: %v", err)
	}
	if got, _ := LogDir(); got != filepath.Join(cache, "dues", "logs") {
		t.Errorf("LogDir = %q, want <Cache>/dues/logs under %q", got, cache)
	}
	temp, err := Temp()
	if err != nil {
		t.Fatal(err)
	}
	want := "/tmp"
	if runtime.GOOS == "windows" {
		want = filepath.Join(cache, "Temp")
	}
	if temp != want {
		t.Errorf("Temp = %q, want %q", temp, want)
	}
}

func TestTempOr(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if got, err := TempOr(dir); err != nil || got != dir {
		t.Errorf("TempOr(%q) = %q, %v; want the flag's directory", dir, got, err)
	}
	temp, terr := Temp()
	got, err := TempOr("")
	if got != temp || fmt.Sprint(err) != fmt.Sprint(terr) {
		t.Errorf("TempOr(\"\") = %q, %v; want Temp() = %q, %v", got, err, temp, terr)
	}
}

func TestAbsolute(t *testing.T) {
	t.Parallel()
	for _, dir := range []string{"", "relative", "." + string(filepath.Separator) + "x"} {
		if _, err := absolute("test folder", dir); err == nil || !strings.Contains(err.Error(), "test folder") {
			t.Errorf("absolute(%q) = %v, want an error naming the folder", dir, err)
		}
	}
}

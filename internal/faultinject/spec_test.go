package faultinject

import (
	"strings"
	"testing"
	"time"
)

func TestParseSpec(t *testing.T) {
	rules, err := ParseSpec(" ingest.before_commit=error , restore.mid=sleep:20ms@3+, reset.mid=exit:7@2,index.mid=signal:INT ")
	if err != nil {
		t.Fatal(err)
	}
	want := []Rule{
		{Point: IngestBeforeCommit, Action: "error"},
		{Point: RestoreMid, Action: "sleep", Delay: 20 * time.Millisecond, Nth: 3, AndAll: true},
		{Point: ResetMid, Action: "exit", Code: 7, Nth: 2},
		{Point: IndexMid, Action: "signal", Signal: "INT"},
	}
	if len(rules) != len(want) {
		t.Fatalf("got %d rules, want %d", len(rules), len(want))
	}
	for i := range want {
		if rules[i] != want[i] {
			t.Errorf("rule %d = %+v, want %+v", i, rules[i], want[i])
		}
	}
	if r, err := ParseSpec(""); err != nil || len(r) != 0 {
		t.Fatalf("empty spec: %v %v", r, err)
	}
}

func TestParseSpecRejects(t *testing.T) {
	for _, spec := range []string{
		"nosuch.point=error",
		"ingest.before_commit",
		"ingest.before_commit=",
		"=error",
		"ingest.before_commit=explode",
		"ingest.before_commit=error:x",
		"ingest.before_commit=exit",
		"ingest.before_commit=exit:300",
		"ingest.before_commit=sleep:soon",
		"ingest.before_commit=signal:HUP",
		"ingest.before_commit=error@0",
		"ingest.before_commit=error@x+",
	} {
		if _, err := ParseSpec(spec); err == nil {
			t.Errorf("ParseSpec(%q) accepted a bad spec", spec)
		}
	}
}

func TestRuleFires(t *testing.T) {
	every := Rule{}
	nth := Rule{Nth: 2}
	from := Rule{Nth: 2, AndAll: true}
	for hit, want := range map[int][3]bool{1: {true, false, false}, 2: {true, true, true}, 3: {true, false, true}} {
		got := [3]bool{every.fires(hit), nth.fires(hit), from.fires(hit)}
		if got != want {
			t.Errorf("hit %d: fires = %v, want %v", hit, got, want)
		}
	}
}

func TestPointsAreUniqueAndDotted(t *testing.T) {
	seen := map[string]bool{}
	for _, p := range Points() {
		if seen[p] {
			t.Errorf("duplicate point %q", p)
		}
		seen[p] = true
		if !strings.Contains(p, ".") || strings.ContainsAny(p, "=,@: ") {
			t.Errorf("point %q must be dotted and free of spec syntax", p)
		}
	}
}

func FuzzParseSpec(f *testing.F) {
	f.Add("ingest.before_commit=error@2+")
	f.Add("restore.mid=sleep:1ms,reset.mid=exit:3")
	f.Add("a=b=c,,@")
	f.Fuzz(func(t *testing.T, spec string) {
		rules, err := ParseSpec(spec)
		if err != nil {
			return
		}
		for _, r := range rules {
			if !Known(r.Point) || r.Nth < 0 || r.Code < 0 || r.Code > 255 || r.Delay < 0 {
				t.Fatalf("accepted invalid rule %+v from %q", r, spec)
			}
		}
	})
}

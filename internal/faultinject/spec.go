package faultinject

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Rule is one parsed entry of a DUES_FAULTS spec.
type Rule struct {
	Point  string
	Action string        // error, enospc, eio, eacces, panic, exit, sleep, block, signal
	Code   int           // exit code for "exit"
	Delay  time.Duration // for "sleep"
	Signal string        // INT or TERM for "signal"
	Nth    int           // fire on this hit (1-based); 0 means every hit
	AndAll bool          // with Nth: fire on the Nth and every later hit
}

// fires reports whether the rule acts on the hit-th hit of its point.
func (r Rule) fires(hit int) bool {
	switch {
	case r.Nth == 0:
		return true
	case r.AndAll:
		return hit >= r.Nth
	default:
		return hit == r.Nth
	}
}

// ParseSpec parses a DUES_FAULTS value. It is available in every build, so a
// release binary can still reject a malformed spec in its own tests.
func ParseSpec(spec string) ([]Rule, error) {
	var rules []Rule
	for entry := range strings.SplitSeq(spec, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		point, action, ok := strings.Cut(entry, "=")
		if !ok || point == "" || action == "" {
			return nil, fmt.Errorf("faultinject: %q: want point=action", entry)
		}
		if !Known(point) {
			return nil, fmt.Errorf("faultinject: %q: unknown point %q", entry, point)
		}
		r := Rule{Point: point}
		if a, nth, ok := strings.Cut(action, "@"); ok {
			action = a
			if strings.HasSuffix(nth, "+") {
				r.AndAll = true
				nth = strings.TrimSuffix(nth, "+")
			}
			n, err := strconv.Atoi(nth)
			if err != nil || n < 1 {
				return nil, fmt.Errorf("faultinject: %q: hit count must be a positive integer", entry)
			}
			r.Nth = n
		}
		name, arg, hasArg := strings.Cut(action, ":")
		r.Action = name
		switch name {
		case "error", "enospc", "eio", "eacces", "panic", "block":
			if hasArg {
				return nil, fmt.Errorf("faultinject: %q: action %s takes no argument", entry, name)
			}
		case "exit":
			code, err := strconv.Atoi(arg)
			if !hasArg || err != nil || code < 0 || code > 255 {
				return nil, fmt.Errorf("faultinject: %q: exit needs a code 0-255", entry)
			}
			r.Code = code
		case "sleep":
			d, err := time.ParseDuration(arg)
			if !hasArg || err != nil || d < 0 {
				return nil, fmt.Errorf("faultinject: %q: sleep needs a duration", entry)
			}
			r.Delay = d
		case "signal":
			if arg != "INT" && arg != "TERM" {
				return nil, fmt.Errorf("faultinject: %q: signal must be INT or TERM", entry)
			}
			r.Signal = arg
		default:
			return nil, fmt.Errorf("faultinject: %q: unknown action %q", entry, name)
		}
		rules = append(rules, r)
	}
	return rules, nil
}

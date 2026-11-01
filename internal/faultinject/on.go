//go:build faultinject

package faultinject

import (
	"fmt"
	"os"
	"sync"
	"time"
)

// Enabled reports whether this build can inject faults.
const Enabled = true

// config is what the process injects: the rules by point, and the file the
// first hit of each point is reported to ("" for none).
type config struct {
	rules  map[string][]Rule
	notify string
}

func newConfig(spec, notify string) (*config, error) {
	parsed, err := ParseSpec(spec)
	if err != nil {
		return nil, err
	}
	c := &config{rules: map[string][]Rule{}, notify: notify}
	for _, r := range parsed {
		c.rules[r.Point] = append(c.rules[r.Point], r)
	}
	return c, nil
}

var (
	mu       sync.Mutex
	active   *config // nil injects nothing
	hits     = map[string]int{}
	notified = map[string]bool{}
)

// Configure installs spec, a list of point=action rules, and the file each
// point's first hit is reported to ("" for none). The binaries call it with
// their hidden --faults and --faults-notify flags, before any work starts. It
// also forgets the hit counts. A bad spec is returned and changes nothing.
func Configure(spec, notify string) error {
	c, err := newConfig(spec, notify)
	if err != nil {
		return err
	}
	install(c)
	return nil
}

// Set installs spec for a test in this process and returns the function
// that puts back what was installed before. Hit counts start again at both.
func Set(spec string) (restore func(), err error) {
	c, err := newConfig(spec, "")
	if err != nil {
		return nil, err
	}
	mu.Lock()
	prev := active
	mu.Unlock()
	install(c)
	return func() { install(prev) }, nil
}

func install(c *config) {
	mu.Lock()
	defer mu.Unlock()
	active = c
	hits = map[string]int{}
	notified = map[string]bool{}
}

// Hit marks a fault point and performs whatever the configuration sets there.
func Hit(point string) error {
	if !Known(point) {
		panic(fmt.Sprintf("faultinject: Hit(%q): unregistered point", point))
	}

	mu.Lock()
	c := active
	hits[point]++
	n := hits[point]
	first := !notified[point]
	notified[point] = true
	mu.Unlock()
	if c == nil {
		return nil
	}
	if first {
		c.report(point)
	}

	for _, r := range c.rules[point] {
		if !r.fires(n) {
			continue
		}
		switch r.Action {
		case "error":
			return fmt.Errorf("%s: %w", point, ErrInjected)
		case "enospc":
			return fmt.Errorf("%s: %w", point, errNoSpace)
		case "eio":
			return fmt.Errorf("%s: %w", point, errIO)
		case "eacces":
			return fmt.Errorf("%s: %w", point, errAccess)
		case "panic":
			panic("faultinject: panic at " + point)
		case "exit":
			os.Exit(r.Code)
		case "sleep":
			time.Sleep(r.Delay)
		case "block":
			c.report(point + "#blocked")
			select {}
		case "signal":
			if err := signalSelf(r.Signal); err != nil {
				return fmt.Errorf("%s: signal %s: %w", point, r.Signal, err)
			}
			// Give the signal handler time to run before the caller goes on.
			time.Sleep(50 * time.Millisecond)
		}
	}
	return nil
}

// report appends a line to the notification file, if there is one.
// Failures are ignored: notification only helps a waiting parent.
func (c *config) report(line string) {
	if c.notify == "" {
		return
	}
	f, err := os.OpenFile(c.notify, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	_, _ = f.WriteString(line + "\n")
	_ = f.Sync()
	_ = f.Close()
}

// Hits reports how many times point has been hit since the last Configure
// or Set.
func Hits(point string) int {
	mu.Lock()
	defer mu.Unlock()
	return hits[point]
}

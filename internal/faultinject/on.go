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

var (
	loadOnce sync.Once
	loadErr  error
	rules    map[string][]Rule

	mu       sync.Mutex
	hits     = map[string]int{}
	notified = map[string]bool{}
)

func load() {
	parsed, err := ParseSpec(os.Getenv("DUES_FAULTS"))
	if err != nil {
		loadErr = err
		return
	}
	rules = map[string][]Rule{}
	for _, r := range parsed {
		rules[r.Point] = append(rules[r.Point], r)
	}
}

// Reset forgets hit counts and re-reads DUES_FAULTS. Tests that change the
// environment in-process call it.
func Reset() {
	mu.Lock()
	defer mu.Unlock()
	hits = map[string]int{}
	notified = map[string]bool{}
	loadOnce = sync.Once{}
	loadErr = nil
	rules = nil
}

// Hit marks a fault point and performs whatever DUES_FAULTS configures there.
func Hit(point string) error {
	loadOnce.Do(load)
	if loadErr != nil {
		// A bad spec is a test bug; fail loudly at the first point reached.
		panic(loadErr)
	}
	if !Known(point) {
		panic(fmt.Sprintf("faultinject: Hit(%q): unregistered point", point))
	}

	mu.Lock()
	hits[point]++
	n := hits[point]
	first := !notified[point]
	notified[point] = true
	mu.Unlock()
	if first {
		notify(point)
	}

	for _, r := range rules[point] {
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
			notify(point + "#blocked")
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

// notify appends a line to the DUES_FAULTS_NOTIFY file, if one is set.
// Failures are ignored: notification only helps a waiting parent.
func notify(line string) {
	path := os.Getenv("DUES_FAULTS_NOTIFY")
	if path == "" {
		return
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	_, _ = f.WriteString(line + "\n")
	_ = f.Sync()
	_ = f.Close()
}

// Hits reports how many times point has been hit since the last Reset.
func Hits(point string) int {
	mu.Lock()
	defer mu.Unlock()
	return hits[point]
}

package ui

import (
	"strconv"
	"sync/atomic"
	"time"
)

// Windows Terminal and ConEmu show a command's progress on its tab and in
// the taskbar when told with OSC 9;4. DUES tells only the terminals known
// to listen (taskbarTerminal), at most twice a second, and clears it on
// every way out: the end of the command, a second Ctrl-C, a panic.

type tbState int

const (
	tbClear tbState = iota
	tbNormal
	tbError
	tbIndeterminate
	tbPaused
)

// osc94 is the sequence that sets the tab's progress, ended by ST.
func osc94(st tbState, pct int) string {
	return "\x1b]9;4;" + strconv.Itoa(int(st)) + ";" + strconv.Itoa(pct) + "\x1b\\"
}

// taskbarShown is set while some display has progress on the tab, so the
// teardown can clear what a display could not.
var taskbarShown atomic.Bool

// taskbar is what a display last sent: nothing is sent twice, and the
// percentage changes at most every taskbarEvery.
type taskbar struct {
	st   tbState
	pct  int
	at   time.Time
	sent bool
}

const taskbarEvery = 500 * time.Millisecond

// next is the sequence that moves the tab to st at pct, or "" when there is
// nothing to send yet. A new state goes at once.
func (t *taskbar) next(st tbState, pct int, now time.Time) string {
	pct = min(max(pct, 0), 100)
	if st == tbIndeterminate {
		pct = 0
	}
	switch {
	case t.sent && st == t.st && pct == t.pct:
		return ""
	case t.sent && st == t.st && now.Sub(t.at) < taskbarEvery:
		return ""
	}
	t.st, t.pct, t.at, t.sent = st, pct, now, true
	taskbarShown.Store(true)
	return osc94(st, pct)
}

// clear is the sequence that takes the progress off the tab, if any was
// shown.
func (t *taskbar) clear() string {
	if !t.sent || t.st == tbClear {
		return ""
	}
	t.st, t.pct = tbClear, 0
	taskbarShown.Store(false)
	return osc94(tbClear, 0)
}

// Package procstat parses /proc/<pid>/stat — the ONE parser muster-mirror
// has: the process table the locators and the paste guard read
// (internal/mirrorhost), and the Hermes hook's record of the gateway that ran
// it (cmd/muster-mirror).
package procstat

import (
	"bytes"
	"strconv"
	"strings"
)

// Stat is the little of a stat line muster-mirror reads.
type Stat struct {
	// Comm is the process name (field 2, without its parentheses).
	Comm string
	// State is field 3 ("S", "R", "Z" for a zombie, …).
	State string
	// PPID is field 4.
	PPID int
	// TTY is field 7, tty_nr: the controlling terminal's device number, 0
	// for none (a process that called setsid, a daemon). 0 also when the
	// line is too short to hold it.
	TTY int
	// Start is field 22, the start time in clock ticks since boot, as
	// written: what tells a live process from a reused pid. "" when absent.
	Start string
}

// Parse reads a stat line. comm can hold spaces and ')', so the fields after
// it are found from the LAST ')'. false for a line that is not one.
func Parse(stat []byte) (Stat, bool) {
	open, close := bytes.IndexByte(stat, '('), bytes.LastIndexByte(stat, ')')
	if open < 0 || close < open {
		return Stat{}, false
	}
	rest := strings.Fields(string(stat[close+1:]))
	if len(rest) < 2 {
		return Stat{}, false
	}
	ppid, _ := strconv.Atoi(rest[1])
	s := Stat{Comm: string(stat[open+1 : close]), State: rest[0], PPID: ppid}
	if len(rest) > 4 {
		s.TTY, _ = strconv.Atoi(rest[4])
	}
	if len(rest) > 19 {
		s.Start = rest[19]
	}
	return s, true
}

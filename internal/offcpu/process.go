package offcpu

import (
	"sort"
	"time"
)

// Process is the threads of one process, added up.
//
// The sum is in thread-seconds and not in wall clock, and confusing the two
// is the mistake this type exists to make hard to make. A Go service with
// nineteen threads observed for ten seconds has a hundred and ninety
// thread-seconds to account for, and it will report most of them as blocked
// whether it served a million requests or none: idle threads are blocked
// threads. "Blocked 96%" is a statement about a thread pool, not about
// latency, and a reader who takes it for the second has been misled by a
// number that is arithmetically correct.
type Process struct {
	// TGID is the process id as the initial pid namespace numbers it. Zero
	// collects every thread whose process is not known yet -- they are kept
	// rather than dropped, because silently losing threads from a total is
	// the failure this whole report is arranged against.
	TGID uint32
	// Comm is the process's name: the group leader's if the leader was
	// observed, and otherwise the name most of its threads carry. A process
	// that names its threads individually -- which a Go runtime does not --
	// can have no single answer, and the majority is the least surprising
	// one.
	Comm string
	// Threads is how many of this process's threads were observed, and
	// Exited how many of those were gone by the end.
	Threads int
	Exited  int

	// Observed is the sum of the threads' observation windows: the
	// thread-seconds this decomposition accounts for.
	Observed time.Duration

	OnCPU     time.Duration
	Runqueue  time.Duration
	Throttled time.Duration
	Blocked   time.Duration
	Unknown   time.Duration
}

// Accounted is the sum of the categories, which by construction equals
// Observed.
func (p Process) Accounted() time.Duration {
	return p.OnCPU + p.Runqueue + p.Throttled + p.Blocked + p.Unknown
}

// Residual is what the categories failed to account for.
func (p Process) Residual() time.Duration { return p.Observed - p.Accounted() }

// Rollup groups threads into the processes they belong to, busiest first,
// where busy means the most time spent somewhere other than running.
//
// Threads whose process is not known yet are collected under process zero
// rather than discarded. A thread is only told its process when an event that
// knows about it arrives, so this bucket is where a thread that has been woken
// but not yet switched out sits for a moment; leaving it out would make the
// totals depend on when the report happened to be taken.
func Rollup(threads []Thread) []Process {
	type group struct {
		process Process
		comms   map[string]int
		leader  string
	}
	groups := make(map[uint32]*group)

	for _, t := range threads {
		g, ok := groups[t.TGID]
		if !ok {
			g = &group{
				process: Process{TGID: t.TGID},
				comms:   make(map[string]int),
			}
			groups[t.TGID] = g
		}
		g.comms[t.Comm]++
		if t.TID == t.TGID {
			g.leader = t.Comm
		}

		g.process.Threads++
		if t.Exited {
			g.process.Exited++
		}
		g.process.Observed += t.Observed
		g.process.OnCPU += t.OnCPU
		g.process.Runqueue += t.Runqueue
		g.process.Throttled += t.Throttled
		g.process.Blocked += t.Blocked
		g.process.Unknown += t.Unknown
	}

	out := make([]Process, 0, len(groups))
	for _, g := range groups {
		g.process.Comm = g.leader
		if g.process.Comm == "" {
			best := 0
			for comm, count := range g.comms {
				// Ties broken by name, so that two runs over the same
				// process do not disagree about what it is called.
				if count > best || (count == best && comm < g.process.Comm) {
					g.process.Comm, best = comm, count
				}
			}
		}
		out = append(out, g.process)
	}

	sort.Slice(out, func(i, j int) bool {
		wi := out[i].Runqueue + out[i].Throttled + out[i].Blocked
		wj := out[j].Runqueue + out[j].Throttled + out[j].Blocked
		if wi != wj {
			return wi > wj
		}
		return out[i].TGID < out[j].TGID
	})
	return out
}

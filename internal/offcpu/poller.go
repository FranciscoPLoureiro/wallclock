package offcpu

import "time"

// Poller is one process's event-loop waiting, and how it is spread across the
// threads doing it.
//
// The shape matters more than the total. Two processes can spend the same
// time in epoll and mean opposite things by it:
//
//   - a dedicated poller -- one thread whose whole life is the event loop,
//     handing work to others. Redis is one thread; an nginx worker owns its
//     own loop; a Java NIO service has selector threads with names. In every
//     case a thread *is* the poller, and it is the same thread all day.
//   - a rotating poller -- a pool of interchangeable threads, any of which
//     may be the one in epoll at a given moment. This is what a Go runtime
//     looks like, and it is why "find the netpoller thread" is not a question
//     with an answer.
//
// Measured on a Go service with sixteen concurrent clients, every one of its
// nineteen threads reported the same thing to within a percentage point:
// about 90% of its blocked time in futex, 5 to 7% in the poller, and under a
// millisecond on sockets. No thread was the netpoller. Every thread was the
// netpoller sometimes.
//
// That is what this type is for: the report cannot label the netpoller thread
// because there is not one, so it labels the process instead and says the
// polling rotates.
type Poller struct {
	TGID uint32
	// Threads is how many threads of this process had any blocked time, and
	// Polling how many of those spent any of it in an event loop.
	Threads int
	Polling int

	// Poll is event-loop waiting summed over the process, Network is waiting
	// on a socket, and Blocked is all of it. Network is here because in a
	// rotating pool it is the number that is conspicuous by its absence, and
	// a reader should not have to go and find it somewhere else.
	Poll    time.Duration
	Network time.Duration
	Blocked time.Duration

	// Dedication is the share of this process's polling done by threads that
	// are dedicated to polling -- that is, threads for which the event loop
	// is most of what they wait on. Zero means the loop belongs to nobody;
	// one means it belongs to somebody. See Rotating.
	Dedication float64

	// Busiest is the largest share of Poll held by any single thread, from 0
	// to 1. Reported because it is what a reader wants to know, and
	// deliberately *not* what Rotating is decided on: see the note on
	// dedicatedThreadShare.
	Busiest float64
}

// The thresholds, and the measurements behind them.
//
// The obvious criterion is the share of the polling that the busiest thread
// holds -- if nobody holds much, the role rotates. It was implemented that
// way first and it is wrong, because it degrades with the size of the pool
// rather than with the thing being measured. The same Go program, doing the
// same work, was run against GOMAXPROCS of 16, 4 and 2:
//
//	GOMAXPROCS   threads polling   busiest share   dedicated share
//	        16       16 of 18            22.4%                  0%
//	         4        6 of 8             35.3%                  0%
//	         2        3 of 5             50.5%                  0%
//
// With fewer processors there are fewer threads to rotate through, so each
// holds more of an unchanged rotation, and at GOMAXPROCS=2 a plainly rotating
// runtime crossed a half-way threshold. The criterion was measuring how big
// the machine is.
//
// What does not move with the pool is how dedicated a thread is to polling.
// A thread that *is* an event loop spends nearly all its blocked time in one;
// a Go M spends 5 to 7% of its blocked time there and the rest parked in
// futex, whether there are three of it or nineteen. So a thread counts as a
// dedicated poller when at least half its own waiting is the loop, and a
// process is rotating when less than half of its polling is done by such
// threads. That is the last column, and it is zero across the whole range.
//
// Taken over the polling rather than over the threads, deliberately. A thread
// seen exactly once, in epoll, has a ratio of 1.0 on a single row and would
// drag a maximum-over-threads test to "dedicated" on the strength of a
// microsecond. Weighting by time makes a thread's vote worth what it waited.
const (
	rotatingMinThreads    = 3
	dedicatedThreadShare  = 0.5
	rotatingMaxDedication = 0.5
)

// Rotating reports that the polling is a role passed around a pool of
// interchangeable threads rather than the job of any one of them.
//
// When this is true of a process, three things follow, and the report says
// all three rather than leaving the reader to work them out:
//
//   - no thread in it can be labelled "the netpoller", because the role moves;
//   - its threads' blocked time is mostly the pool being idle, not the
//     process waiting for anything;
//   - its network waiting is not in this report at all, because a runtime
//     that parks goroutines on descriptors never leaves a thread on one.
func (p Poller) Rotating() bool {
	return p.Polling >= rotatingMinThreads && p.Dedication < rotatingMaxDedication
}

// DedicatedPollers names the threads whose waiting is mostly an event loop.
//
// This is the thread-level half of the same question, and it is the half that
// has an answer only sometimes. Where a thread *is* the poller -- Redis's one
// thread, an nginx worker, a named selector thread -- it is here, and the
// report marks it so its blocked time is not read as a slow dependency: a
// thread in epoll is waiting to be given work, not waiting for work to
// finish. Where the poller is a role that rotates, this is empty, and that
// emptiness is the finding rather than a failure to look.
func DedicatedPollers(blocked []Blocked) map[uint32]bool {
	type totals struct{ poll, blocked time.Duration }
	perTID := make(map[uint32]*totals)
	for _, b := range blocked {
		t, ok := perTID[b.TID]
		if !ok {
			t = &totals{}
			perTID[b.TID] = t
		}
		t.blocked += b.Duration
		if b.Reason == ReasonPoll {
			t.poll += b.Duration
		}
	}

	out := make(map[uint32]bool)
	for tid, t := range perTID {
		if t.poll > 0 && t.blocked > 0 &&
			float64(t.poll)/float64(t.blocked) >= dedicatedThreadShare {
			out[tid] = true
		}
	}
	return out
}

// Pollers summarises the event-loop waiting of every process the blocked rows
// belong to.
//
// The rows are the same ones the reason breakdown is built from, so this costs
// a second pass over data already in hand and no extra reading of the kernel.
// Threads whose process is not yet known are grouped under zero, exactly as
// Rollup groups them, so the two views agree about who exists.
func Pollers(blocked []Blocked, threads []Thread) map[uint32]Poller {
	process := make(map[uint32]uint32, len(threads))
	for _, t := range threads {
		process[t.TID] = t.TGID
	}

	// Per process, and within it per thread: neither the share one thread
	// holds nor how dedicated it is can be known until its own totals are.
	type threadTotals struct{ poll, blocked time.Duration }
	type accumulator struct {
		poller Poller
		perTID map[uint32]*threadTotals
	}
	groups := make(map[uint32]*accumulator)

	for _, b := range blocked {
		tgid := process[b.TID]
		g, ok := groups[tgid]
		if !ok {
			g = &accumulator{
				poller: Poller{TGID: tgid},
				perTID: make(map[uint32]*threadTotals),
			}
			groups[tgid] = g
		}
		totals, ok := g.perTID[b.TID]
		if !ok {
			totals = &threadTotals{}
			g.perTID[b.TID] = totals
		}
		totals.blocked += b.Duration
		g.poller.Blocked += b.Duration

		switch b.Reason {
		case ReasonPoll:
			totals.poll += b.Duration
			g.poller.Poll += b.Duration
		case ReasonNetwork:
			g.poller.Network += b.Duration
		case ReasonFutex, ReasonDisk, ReasonSleep, ReasonOther:
			// Counted in Blocked above and not separately here. This type
			// answers one question and the reason breakdown answers the rest.
		}
	}

	out := make(map[uint32]Poller, len(groups))
	for tgid, g := range groups {
		g.poller.Threads = len(g.perTID)

		var dedicated time.Duration
		for _, totals := range g.perTID {
			// A thread that appears with no polling at all has not polled.
			// Counting it would let a process look spread out because many
			// threads did nothing.
			if totals.poll <= 0 {
				continue
			}
			g.poller.Polling++
			if share := float64(totals.poll) / float64(g.poller.Poll); share > g.poller.Busiest {
				g.poller.Busiest = share
			}
			if totals.blocked > 0 &&
				float64(totals.poll)/float64(totals.blocked) >= dedicatedThreadShare {
				dedicated += totals.poll
			}
		}
		if g.poller.Poll > 0 {
			g.poller.Dedication = float64(dedicated) / float64(g.poller.Poll)
		}
		out[tgid] = g.poller
	}
	return out
}

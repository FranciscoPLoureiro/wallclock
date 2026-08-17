package offcpu_test

import (
	"testing"
	"time"

	"github.com/FranciscoPLoureiro/wallclock/internal/offcpu"
)

// pollRow and futexRow keep the tables below readable: what these tests are
// about is who waited and for how long, not what the stack looked like.
func pollRow(tid uint32, d time.Duration) offcpu.Blocked {
	return offcpu.Blocked{TID: tid, Reason: offcpu.ReasonPoll, Duration: d}
}

func futexRow(tid uint32, d time.Duration) offcpu.Blocked {
	return offcpu.Blocked{TID: tid, Reason: offcpu.ReasonFutex, Duration: d}
}

func threadsOf(tgid uint32, tids ...uint32) []offcpu.Thread {
	out := make([]offcpu.Thread, 0, len(tids))
	for _, tid := range tids {
		out = append(out, offcpu.Thread{TID: tid, TGID: tgid})
	}
	return out
}

func TestRotatingPollersAreTheOnesNoThreadIsDedicatedTo(t *testing.T) {
	const ms = time.Millisecond

	cases := []struct {
		name    string
		blocked []offcpu.Blocked
		threads []offcpu.Thread
		want    bool
		reason  string
	}{
		{
			// The Go shape. Five threads, each parked in futex most of the
			// time and in the poller a little, none of them the poller.
			name: "a pool that passes the loop around",
			blocked: []offcpu.Blocked{
				pollRow(10, 100*ms), futexRow(10, 900*ms),
				pollRow(11, 110*ms), futexRow(11, 890*ms),
				pollRow(12, 90*ms), futexRow(12, 910*ms),
				pollRow(13, 105*ms), futexRow(13, 895*ms),
				pollRow(14, 95*ms), futexRow(14, 905*ms),
			},
			threads: threadsOf(7, 10, 11, 12, 13, 14),
			want:    true,
		},
		{
			// The case that broke the first version of this, and the reason
			// the criterion is dedication rather than share. Three threads
			// of a small runtime: the busiest holds 60% of the polling,
			// which the old rule called a dedicated poller, and not one of
			// them spends more than a tenth of its own waiting in the loop.
			// A Go program on a two-processor machine looks exactly like
			// this and is not a different kind of program.
			name: "a small pool where the busiest still holds most of it",
			blocked: []offcpu.Blocked{
				pollRow(50, 120*ms), futexRow(50, 1080*ms),
				pollRow(51, 40*ms), futexRow(51, 1160*ms),
				pollRow(52, 40*ms), futexRow(52, 1160*ms),
			},
			threads: threadsOf(12, 50, 51, 52),
			want:    true,
			reason:  "no thread waits mostly in the loop",
		},
		{
			// A thread whose job is the event loop, and four that are not.
			name: "one thread that is the event loop",
			blocked: []offcpu.Blocked{
				pollRow(20, 500*ms),
				futexRow(21, 900*ms), futexRow(22, 900*ms),
				futexRow(23, 900*ms), futexRow(24, 900*ms),
			},
			threads: threadsOf(8, 20, 21, 22, 23, 24),
			want:    false,
			reason:  "one thread is the loop and the others never touch it",
		},
		{
			// Three dedicated loops side by side -- a worker pool where each
			// worker owns its own epoll, which is how nginx is arranged.
			// Spread across threads, and not a rotation: no thread ever
			// polls on another's behalf.
			name: "a loop per worker",
			blocked: []offcpu.Blocked{
				pollRow(60, 900*ms), futexRow(60, 100*ms),
				pollRow(61, 880*ms), futexRow(61, 120*ms),
				pollRow(62, 910*ms), futexRow(62, 90*ms),
			},
			threads: threadsOf(13, 60, 61, 62),
			want:    false,
			reason:  "every one of them is a dedicated loop",
		},
		{
			// Two threads is not a pool, whatever they are doing. With two,
			// a share below half is impossible for one of them and certain
			// for the other, so anything decided on the split is measuring
			// arithmetic.
			name: "two threads sharing it",
			blocked: []offcpu.Blocked{
				pollRow(30, 100*ms), futexRow(30, 900*ms),
				pollRow(31, 100*ms), futexRow(31, 900*ms),
			},
			threads: threadsOf(9, 30, 31),
			want:    false,
			reason:  "two is not a pool",
		},
		{
			// One thread seen exactly once, in epoll, beside a real pool.
			// Its own ratio is 1.0 on a single row, and a rule that took the
			// most dedicated thread would call this process dedicated on the
			// strength of a millisecond. Weighting by time is what stops it.
			name: "a stray thread caught mid-poll",
			blocked: []offcpu.Blocked{
				pollRow(70, 100*ms), futexRow(70, 900*ms),
				pollRow(71, 100*ms), futexRow(71, 900*ms),
				pollRow(72, 100*ms), futexRow(72, 900*ms),
				pollRow(73, 1*ms),
			},
			threads: threadsOf(14, 70, 71, 72, 73),
			want:    true,
			reason:  "a millisecond does not make a dedicated poller",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pollers := offcpu.Pollers(tc.blocked, tc.threads)
			tgid := tc.threads[0].TGID
			got, ok := pollers[tgid]
			if !ok {
				t.Fatalf("no summary for process %d", tgid)
			}
			if got.Rotating() != tc.want {
				t.Errorf("Rotating() = %v, want %v (%s): %d of %d threads "+
					"polling, %.0f%% of the polling done by dedicated threads, "+
					"busiest holds %.1f%%",
					got.Rotating(), tc.want, tc.reason,
					got.Polling, got.Threads, 100*got.Dedication, 100*got.Busiest)
			}
		})
	}
}

// The summary is per process, and a machine runs more than one.
func TestPollersKeepProcessesApart(t *testing.T) {
	const ms = time.Millisecond

	blocked := []offcpu.Blocked{
		// A rotating pool in process 100.
		pollRow(10, 100*ms), futexRow(10, 900*ms),
		pollRow(11, 100*ms), futexRow(11, 900*ms),
		pollRow(12, 100*ms), futexRow(12, 900*ms),
		{TID: 10, Reason: offcpu.ReasonNetwork, Duration: 5 * ms},
		// A dedicated poller in process 200, with a thread that really is
		// waiting on a socket beside it.
		pollRow(20, 900*ms),
		{TID: 21, Reason: offcpu.ReasonNetwork, Duration: 700 * ms},
	}
	threads := append(threadsOf(100, 10, 11, 12), threadsOf(200, 20, 21)...)

	pollers := offcpu.Pollers(blocked, threads)
	if len(pollers) != 2 {
		t.Fatalf("got %d processes, want 2", len(pollers))
	}

	pool := pollers[100]
	if !pool.Rotating() {
		t.Errorf("process 100 should be a rotating pool: %d polling, %.0f%% dedicated",
			pool.Polling, 100*pool.Dedication)
	}
	if pool.Poll != 300*ms {
		t.Errorf("process 100 polled for %v, want %v", pool.Poll, 300*ms)
	}
	if pool.Network != 5*ms {
		t.Errorf("process 100 waited on sockets for %v, want %v", pool.Network, 5*ms)
	}
	if pool.Blocked != 3005*ms {
		t.Errorf("process 100 blocked for %v in total, want %v", pool.Blocked, 3005*ms)
	}

	dedicated := pollers[200]
	if dedicated.Rotating() {
		t.Error("process 200 has one poller and one thread on a socket, which is " +
			"not a rotation")
	}
	if dedicated.Dedication != 1 {
		t.Errorf("process 200 does %.0f%% of its polling on dedicated threads, want all of it",
			100*dedicated.Dedication)
	}
	if dedicated.Network != 700*ms {
		t.Errorf("process 200 waited on sockets for %v, want %v",
			dedicated.Network, 700*ms)
	}
	if dedicated.Threads != 2 {
		t.Errorf("process 200 had %d threads blocked, want 2", dedicated.Threads)
	}
}

// A process nothing is known about yet still has to be counted.
//
// A thread learns which process it belongs to from the first event able to
// say so, so between a wakeup and the switch-out that follows it there is a
// moment where the answer is zero. Dropping those rows would make the totals
// depend on when the report was taken.
func TestPollersKeepThreadsWhoseProcessIsUnknown(t *testing.T) {
	blocked := []offcpu.Blocked{pollRow(50, time.Second)}

	pollers := offcpu.Pollers(blocked, []offcpu.Thread{{TID: 50, TGID: 0}})
	unknown, ok := pollers[0]
	if !ok {
		t.Fatal("the row was dropped rather than collected under process zero")
	}
	if unknown.Poll != time.Second {
		t.Errorf("collected %v, want %v", unknown.Poll, time.Second)
	}

	// And a thread with no row at all in the thread list is not invented.
	if len(pollers) != 1 {
		t.Errorf("got %d processes, want 1", len(pollers))
	}
}

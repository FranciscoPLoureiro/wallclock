package offcpu_test

import (
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/FranciscoPLoureiro/wallclock/internal/kerneltest"
	"github.com/FranciscoPLoureiro/wallclock/internal/ksyms"
	"github.com/FranciscoPLoureiro/wallclock/internal/offcpu"
)

// A thread whose very first appearance is going to sleep still has to say
// what it went to sleep on.
//
// The kernel side records a blocked thread's stack on the transition into
// blocked -- except on the transition that *creates* the entry, where the
// fresh record was zeroed and its stack id set to "none" regardless of what
// the thread was about to do. The time was counted correctly; only the
// explanation was dropped.
//
// Which threads does that catch? Not new ones: a thread created by clone is
// announced by sched_wakeup_new and already has an entry by the time it runs.
// It catches threads that were *already on a CPU* when the session opened and
// whose next move was to block -- so, on any machine, whatever was running at
// the instant profiling started. That is a small number of threads and an
// unbounded amount of time, because the wait they begin can last the whole
// window.
//
// Found by the phase 5 network scenario, whose subject connects as the first
// thing it does: exactly one round trip out of twenty-six came back with no
// reason attached.
func TestAThreadThatStartsBlockedStillSaysWhy(t *testing.T) {
	kerneltest.Root(t)

	const (
		subjects = 8
		nap      = 400 * time.Millisecond
		name     = testThreadPrefix + "firstwait"
	)

	// The subjects spin before they sleep, and the session opens in between.
	//
	// That ordering is the whole test. A thread that blocks before the
	// session opens is discovered by its wakeup; a thread created after it
	// opens is discovered by sched_wakeup_new. Only a thread that is holding
	// a CPU when profiling starts, and then stops, arrives at the kernel side
	// as a switch-out with no entry -- which is the path that used to throw
	// the stack away. Spinning is how the subjects are made to hold a CPU.
	var sleepNow atomic.Bool
	var running, finished sync.WaitGroup
	running.Add(subjects)
	finished.Add(subjects)

	for range subjects {
		go func() {
			defer finished.Done()
			nameThisThread(t, name)
			defer runtime.UnlockOSThread()

			running.Done()
			for !sleepNow.Load() { //nolint:revive // holding a CPU is the point
			}

			// nanosleep through the syscall, not time.Sleep: time.Sleep parks
			// the goroutine and leaves the thread free to run something else,
			// so no OS thread would ever block and there would be nothing to
			// measure.
			ts := unix.NsecToTimespec(nap.Nanoseconds())
			for {
				err := unix.Nanosleep(&ts, &ts)
				if err == nil {
					return
				}
				if err != unix.EINTR {
					t.Errorf("nanosleep: %v", err)
					return
				}
			}
		}()
	}
	running.Wait()

	session, err := offcpu.Open(0)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer session.Close()

	sleepNow.Store(true)
	finished.Wait()

	symbols, err := ksyms.Load()
	if err != nil {
		t.Fatalf("load kernel symbols: %v", err)
	}
	// Reasons first, then totals, then the open waits: the other order counts
	// an interval that ends in between twice. See BlockedReasons.
	blocked, err := session.BlockedReasons(symbols)
	if err != nil {
		t.Fatalf("blocked reasons: %v", err)
	}
	threads, err := session.Threads()
	if err != nil {
		t.Fatalf("threads: %v", err)
	}
	if blocked, err = session.AttributeOpenWaits(blocked, threads, symbols); err != nil {
		t.Fatalf("attribute open waits: %v", err)
	}

	var (
		seen         int
		totalBlocked time.Duration
		unattributed time.Duration
		asSleep      time.Duration
	)
	for _, thread := range threads {
		if thread.Comm != name {
			continue
		}
		seen++
		totalBlocked += thread.Blocked
		reasons, missing := offcpu.ByReason(blocked, thread)
		unattributed += missing
		asSleep += reasons[offcpu.ReasonSleep]
	}
	if seen == 0 {
		t.Fatalf("no thread named %q among %d observed", name, len(threads))
	}
	t.Logf("%d threads, blocked %v, of which sleep %v and unattributed %v",
		seen, totalBlocked, asSleep, unattributed)

	if totalBlocked == 0 {
		t.Fatal("the subjects were never seen blocked at all")
	}

	// An absolute bound rather than a share, and it can be this tight because
	// nothing here is open when the maps are read. The one honest source of
	// unattributed time -- a wait that ends between the two reads, which is
	// documented on BlockedReasons -- cannot arise: every subject has
	// finished before the first read begins. So the expected value is zero,
	// and the allowance is for a stack that could not be captured rather than
	// for timing.
	//
	// Against the code before the fix this reports one whole nap per thread
	// that started on a CPU, which measured 400 ms of 4.1 s.
	const allowed = 20 * time.Millisecond
	if unattributed > allowed {
		t.Errorf("%v of these threads' blocked time has no reason attached, out "+
			"of %v: a thread whose first observed event is a switch-out is losing "+
			"the stack it stopped on", unattributed, totalBlocked)
	}
	if asSleep < totalBlocked/2 {
		t.Errorf("only %v of %v was filed as sleep, and these threads did nothing "+
			"but nanosleep", asSleep, totalBlocked)
	}
}

package offcpu_test

import (
	"os"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/FranciscoPLoureiro/wallclock/internal/kerneltest"
	"github.com/FranciscoPLoureiro/wallclock/internal/offcpu"
)

// The per-thread state in the kernel maps has no synchronisation of its own,
// and the argument for that is in bpf/offcpu.bpf.c: the accumulators are
// plain additions, which are correct for a thread because a thread runs on
// one CPU at a time.
//
// That argument does not cover the whole of it. `sched_wakeup` fires on the
// *waker's* CPU and touches the *sleeper's* entry, so two CPUs can be inside
// one thread's state at once, and what keeps them apart is the kernel
// serialising the wakeup against the context switch through the runqueue
// locks. That was believed and never tested.
//
// A violation would not be a crash. It would be arithmetic that cannot be
// true: a thread accounting for more or less time than it was observed for.
// So this generates the worst interleaving available -- pairs of threads
// handing a byte back and forth through a pipe, one pair per CPU, so that
// almost every wakeup crosses from one core to another -- and checks the
// arithmetic against the same tolerance the quiet test uses.
//
// It is evidence rather than proof: a race that does not happen in three
// seconds has not been shown to be impossible. What it rules out is the
// version where the interleaving is common enough to be seen at all.
func TestTheDecompositionHoldsUnderCrossCPUWakeups(t *testing.T) {
	kerneltest.Root(t)

	stop := make(chan struct{})
	var wg sync.WaitGroup
	for range runtime.NumCPU() {
		startPingPong(t, &wg, stop)
	}
	defer func() {
		close(stop)
		wg.Wait()
	}()

	// Already exchanging before the session opens. Go hands out threads it
	// already has rather than fresh ones, so a session opened first would
	// spend its window watching threads being created, which is a different
	// measurement and a quieter one.
	time.Sleep(300 * time.Millisecond)

	session, err := offcpu.Open(0)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer session.Close()

	time.Sleep(3 * time.Second)

	threads, err := session.Threads()
	if err != nil {
		t.Fatalf("threads: %v", err)
	}
	if len(threads) == 0 {
		t.Fatal("no threads were observed at all, on a machine running this test")
	}

	for _, thread := range threads {
		assertDecompositionCloses(t, thread)
		// Separately from the residual, because a negative component and a
		// residual that happens to cancel out are different bugs and only one
		// of them is visible in a sum.
		for name, d := range map[string]time.Duration{
			"on-CPU":    thread.OnCPU,
			"runqueue":  thread.Runqueue,
			"throttled": thread.Throttled,
			"blocked":   thread.Blocked,
			"unknown":   thread.Unknown,
		} {
			if d < 0 {
				t.Errorf("thread %d (%s): %s is %v, which is not a length of time",
					thread.TID, thread.Comm, name, d)
			}
			if d > thread.Observed {
				t.Errorf("thread %d (%s): %s is %v out of %v observed",
					thread.TID, thread.Comm, name, d, thread.Observed)
			}
		}
	}
	t.Logf("%d threads under %d pipe pairs on %d CPUs, every decomposition closing",
		len(threads), runtime.NumCPU(), runtime.NumCPU())
}

// startPingPong hands a byte back and forth between two threads through a
// pair of pipes. Each side blocks in read and is woken by the other, which is
// the cross-CPU wakeup this is here to generate, and it does it thousands of
// times a second without spending any CPU on work.
func startPingPong(t *testing.T, wg *sync.WaitGroup, stop <-chan struct{}) {
	t.Helper()

	there, hereWrite, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	back, thereWrite, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}

	side := func(in *os.File, out *os.File, opener bool) {
		defer wg.Done()
		// Pinned, so that the thread the kernel sees is the thread this
		// goroutine keeps. Unpinned, the runtime is free to move the work and
		// the entry being contended is no longer the same one.
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()

		buf := make([]byte, 1)
		if opener {
			if _, err := out.Write(buf); err != nil {
				return
			}
		}
		for {
			select {
			case <-stop:
				return
			default:
			}
			if _, err := in.Read(buf); err != nil {
				return
			}
			if _, err := out.Write(buf); err != nil {
				return
			}
		}
	}

	wg.Add(2)
	go side(there, thereWrite, false)
	go side(back, hereWrite, true)

	t.Cleanup(func() {
		for _, f := range []*os.File{there, hereWrite, back, thereWrite} {
			_ = f.Close()
		}
	})
}

package validate

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"

	"github.com/FranciscoPLoureiro/wallclock/internal/offcpu"
)

// The shape of the contention, chosen so the answer is division.
//
// N threads take turns holding one lock, all of them from the same instant
// until the same deadline. Only one can hold it at a time, so at every moment
// one thread is running and N-1 are waiting: over a window of W, the waiting
// adds up to (N-1) x W no matter who wins which race.
//
// Until the deadline, and not for a fixed number of turns, and the difference
// is not cosmetic. A futex lock is not fair -- a thread that has just released
// can win the race to re-acquire while the woken sleeper is still on its way
// back -- so with a fixed count the threads finish at wildly different times,
// and every one that finishes early stops participating. Measured that way
// the total came out at 3.34s against 6s, because a third of the arithmetic
// described threads that had already gone home.
const (
	futexThreads   = 3
	futexHoldTime  = 20 * time.Millisecond
	futexWaitTotal = (futexThreads - 1) * window
)

// futexScenario: N threads deliberately contending for one lock.
//
// The lock is a raw futex on pinned threads, and that is forced rather than
// chosen -- for the same reason the network scenario uses a raw socket. A Go
// sync.Mutex parks the *goroutine* and leaves its thread free to run another
// one, so a hundred goroutines fighting over a mutex produce no thread-level
// contention at all. What futex time such a program does show is its idle Ms
// parked with nothing to do, which is a different quantity that happens to
// land in the same column. Phase 3 says the kernel cannot tell those apart;
// this scenario therefore has to produce the real one on purpose.
func futexScenario() Scenario {
	return Scenario{
		Name:     "futex contention",
		Category: "blocked (futex)",
		GroundTruth: fmt.Sprintf("%d threads taking turns to hold one lock for %v each, over %v",
			futexThreads, futexHoldTime, window),
		Expected: futexWaitTotal,
		// Short of the arithmetic rather than over it, and for a reason
		// this scenario can see: between releasing the lock and blocking on
		// the next acquire, a thread is briefly running rather than
		// waiting, and it pays that gap once per turn.
		Low:  time.Duration(0.90 * float64(futexWaitTotal)),
		High: time.Duration(1.05 * float64(futexWaitTotal)),
		Run: func() (time.Duration, error) {
			var c contention
			obs, err := observe("futex", true, c.start, c.run)
			if err != nil {
				return 0, err
			}

			// The harness checking itself before the tool is judged on it.
			// The design says the holds add up to the elapsed time, because
			// only one thread can hold at once; if they do not, the subjects
			// were not actually contending and the row below means nothing.
			if drift := c.elapsed - c.held(); drift < 0 || drift > window/10 {
				return 0, fmt.Errorf("the subjects held the lock for %v across %v "+
					"of wall clock: they were not taking turns, so nothing here is "+
					"a measurement of contention", c.held(), c.elapsed)
			}

			// The exact relation, against a clock this tool has nothing to
			// do with. At every instant one thread holds and N-1 wait, so
			// the waiting must add up to N-1 times however long the
			// contention actually lasted -- which is a little more than the
			// nominal window, since a thread only checks the deadline after
			// letting go. The table above compares against the design
			// value; this compares against what happened, and is an error
			// rather than a tolerance because it is arithmetic.
			available := time.Duration(futexThreads-1) * c.elapsed
			futex := obs.blockedFor(offcpu.ReasonFutex)
			if ratio := float64(futex) / float64(available); ratio < 0.98 || ratio > 1.02 {
				return futex, fmt.Errorf("the threads waited %v, and %d of them "+
					"contending over %v of wall clock leaves room for %v: a ratio "+
					"of %.3f", futex, futexThreads, c.elapsed, available, ratio)
			}

			// Same guard as the network scenario, for the same reason: a
			// shortfall could be misfiling or blindness, and only the
			// breakdown separates them.
			blocked := obs.sum(func(t offcpu.Thread) time.Duration { return t.Blocked })
			if blocked > 0 && float64(futex)/float64(blocked) < 0.9 {
				return futex, fmt.Errorf("only %v of these threads' blocked time was "+
					"filed as futex: %s", futex, obs.describe())
			}
			return futex, nil
		},
	}
}

// contention is futexThreads taking turns to hold one lock.
//
// Split into a setup and a run so that the profiling session can open between
// them. The subjects have to exist, be named, and be *holding a CPU* before
// profiling starts: a thread the Go runtime hands out is one it already had,
// carrying however much history it accumulated earlier in the process, and
// the kernel side matches by name at read time -- so a session opened first
// counts that history against this scenario. It measured 121 ms per thread.
//
// The barrier is a spun-on atomic and not a channel, for the same reason. A
// goroutine waiting on a channel is parked, and its pinned thread has nothing
// to do, so the thread parks in futex -- which is the exact category this
// scenario is trying to measure, arriving before the measurement starts.
type contention struct {
	lock     futexLock
	release  atomic.Bool
	spinning sync.WaitGroup
	done     sync.WaitGroup
	errs     chan error
	holdSum  atomic.Int64
	finished atomic.Int32
	elapsed  time.Duration
}

// start creates the subjects and returns once every one of them is spinning.
func (c *contention) start() error {
	c.errs = make(chan error, futexThreads)
	c.spinning.Add(futexThreads)
	c.done.Add(futexThreads)

	for range futexThreads {
		go func() {
			defer c.done.Done()
			unpin, err := nameThisThread("futex")
			if err != nil {
				c.errs <- err
				c.spinning.Done()
				return
			}
			defer unpin()

			c.spinning.Done()
			for !c.release.Load() { //nolint:revive // holding a CPU is the point
			}

			deadline := time.Now().Add(window)
			for time.Now().Before(deadline) {
				c.lock.acquire()
				// Held by burning CPU rather than by sleeping. A holder that
				// slept would be blocked itself, and its time would land in
				// the same column as the waiting being measured.
				at := time.Now()
				spinFor(futexHoldTime)
				c.holdSum.Add(int64(time.Since(at)))
				c.lock.release()
			}

			// Nobody leaves until everybody has finished, and they spin
			// rather than wait while that happens.
			//
			// Threads finish a hold or two apart, and a thread that returns
			// early hands its OS thread back to the runtime, which parks it
			// in futex -- idle time landing in the very column this scenario
			// measures, in a place the kernel cannot tell from contention.
			// That is the phase 3 ambiguity turning up inside the harness,
			// and it measured as a 5% overcount.
			c.finished.Add(1)
			for c.finished.Load() < futexThreads { //nolint:revive // holding a CPU is the point
			}
		}()
	}
	c.spinning.Wait()

	select {
	case err := <-c.errs:
		return err
	default:
		return nil
	}
}

// run releases the subjects and waits for them to finish.
func (c *contention) run() error {
	began := time.Now()
	c.release.Store(true)
	c.done.Wait()
	c.elapsed = time.Since(began)

	select {
	case err := <-c.errs:
		return err
	default:
		return nil
	}
}

func (c *contention) held() time.Duration { return time.Duration(c.holdSum.Load()) }

// spinFor holds a CPU for d.
func spinFor(d time.Duration) {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) { //nolint:revive // holding a CPU is the point
	}
}

// futexLock is a mutex built directly on the futex syscall.
//
// Deliberately not sync.Mutex. This has to stop an OS *thread*, which is the
// only thing the kernel can see and the only thing this scenario is about;
// Go's mutex parks a goroutine and the thread carries on with other work.
//
// The protocol is the textbook one: the state is zero when free and one when
// held, a contending thread asks the kernel to sleep until it changes, and a
// releasing thread wakes one sleeper. FUTEX_WAIT compares the value under the
// futex lock before sleeping, which is what closes the window between a
// failed acquire and the sleep -- if the holder released in that gap the wait
// returns EAGAIN and the loop tries again.
type futexLock struct {
	state uint32
}

const (
	futexUnlocked = 0
	futexLocked   = 1
)

// The futex operations, from linux/futex.h.
//
// Declared here because x/sys/unix does not export them: what it does export
// is SYS_FUTEX_WAIT and SYS_FUTEX_WAKE, which are the separate syscalls of
// the futex2 interface added in 6.7 and absent from the 6.6 kernel this is
// developed on. The classic multiplexed futex(2) with an operation code is
// available everywhere and is what Go's own runtime uses.
//
// These are numbers in a stable kernel ABI, unlike the tracepoint offsets
// that cost this project a day -- futex operation codes cannot be renumbered
// without breaking every libc in existence.
const (
	futexWaitOp      = 0
	futexWakeOp      = 1
	futexPrivateFlag = 128
)

func (l *futexLock) acquire() {
	for {
		if atomic.CompareAndSwapUint32(&l.state, futexUnlocked, futexLocked) {
			return
		}
		// EAGAIN means the value changed before the kernel could park this
		// thread, and EINTR means a signal arrived; both mean "try again"
		// and neither is an error worth carrying out of here.
		_, _, errno := unix.Syscall6(unix.SYS_FUTEX,
			uintptr(unsafe.Pointer(&l.state)), //nolint:gosec // the futex word, by address
			futexWaitOp|futexPrivateFlag,
			futexLocked, 0, 0, 0)
		if errno != 0 && !errors.Is(errno, unix.EAGAIN) && !errors.Is(errno, unix.EINTR) {
			// Nothing here can act on it, and spinning is still correct --
			// the CAS above is what actually takes the lock.
			continue
		}
	}
}

func (l *futexLock) release() {
	atomic.StoreUint32(&l.state, futexUnlocked)
	// One waiter, not all of them. Waking every sleeper for a lock only one
	// of them can take is the thundering herd, and it would put the wake-up
	// cost of N-1 threads into a measurement of how long they waited.
	_, _, _ = unix.Syscall6(unix.SYS_FUTEX,
		uintptr(unsafe.Pointer(&l.state)), //nolint:gosec // the futex word, by address
		futexWakeOp|futexPrivateFlag,
		1, 0, 0, 0)
}

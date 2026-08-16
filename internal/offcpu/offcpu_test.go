package offcpu_test

import (
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/FranciscoPLoureiro/wallclock/internal/kerneltest"
	"github.com/FranciscoPLoureiro/wallclock/internal/offcpu"
)

// The subjects name themselves.
//
// The alternative is to find them by pid, and the pid a test can see is the
// one its own namespace assigns, while the kernel reports the initial
// namespace's. Inside a container those are different numbers for the same
// thread. A thread name set with prctl is the same string on both sides, so
// the tests locate their subjects by something that does not depend on where
// they are run.
const (
	// testThreadPrefix marks every thread name this package assigns, so the
	// filter test can tell its own subjects from an intruder without keeping
	// a list in step with the tests that create them.
	testThreadPrefix = "wc-"

	sleeperName = testThreadPrefix + "sleeper"
	spinnerName = testThreadPrefix + "spinner"
)

// processComm is this binary's own thread name, captured before any test has
// had a chance to change one.
//
// It cannot be read on demand. /proc/self/comm reports the name of the thread
// group leader, the tests rename threads, and runtime.LockOSThread can hand a
// goroutine the main thread -- so a test that renames its subject can rename
// the process. Reading it later then reports "wc-sleeper" as this binary's
// name and every genuine thread looks like an intruder, which is precisely
// what it did.
var processComm string

func TestMain(m *testing.M) {
	raw, err := os.ReadFile("/proc/self/comm")
	if err != nil {
		panic("cannot read this process's own name: " + err.Error())
	}
	processComm = strings.TrimSpace(string(raw))
	os.Exit(m.Run())
}

// nameThisThread pins the goroutine to its OS thread and names that thread.
//
// The pinning is not decoration: without it the goroutine can be moved to
// another thread mid-test, and the measurement would then be of two threads,
// neither of them completely.
//
// The name is set by writing to /proc/thread-self/comm rather than through
// prctl, which would need an unsafe pointer into a 16-byte buffer to say the
// same thing. thread-self is the calling thread by definition, so this needs
// no pid at all -- which is convenient here, where pids are the thing that
// cannot be trusted to mean the same on both sides.
func nameThisThread(t *testing.T, name string) {
	t.Helper()
	runtime.LockOSThread()
	if err := os.WriteFile("/proc/thread-self/comm", []byte(name), 0o644); err != nil {
		t.Fatalf("naming this thread %q: %v", name, err)
	}
}

func findByComm(threads []offcpu.Thread, comm string) (offcpu.Thread, bool) {
	for _, thread := range threads {
		if thread.Comm == comm {
			return thread, true
		}
	}
	return offcpu.Thread{}, false
}

// A thread that sleeps is blocked, and a profiler that cannot see that is
// blind to most of what a service does. This is the measurement offcputime
// also makes; the ones below are the ones it does not.
func TestASleepingThreadIsBlocked(t *testing.T) {
	kerneltest.Root(t)

	session, err := offcpu.Open(0)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer session.Close()

	const nap = 600 * time.Millisecond
	done := make(chan struct{})
	go func() {
		defer close(done)
		nameThisThread(t, sleeperName)
		defer runtime.UnlockOSThread()
		time.Sleep(nap)
	}()
	<-done

	threads, err := session.Threads()
	if err != nil {
		t.Fatalf("threads: %v", err)
	}
	sleeper, ok := findByComm(threads, sleeperName)
	if !ok {
		t.Fatalf("no thread named %q among %d observed", sleeperName, len(threads))
	}

	// Declared tolerances rather than "approximately". The lower bound is
	// what was asked for minus the slack a sleep is allowed to take on the
	// way in and out; the upper bound is the observation itself, since a
	// thread cannot be blocked for longer than it was watched.
	if sleeper.Blocked < nap-50*time.Millisecond {
		t.Errorf("blocked for %v, want at least %v", sleeper.Blocked, nap-50*time.Millisecond)
	}
	if sleeper.Blocked > sleeper.Observed {
		t.Errorf("blocked for %v across an observation of %v", sleeper.Blocked, sleeper.Observed)
	}
	// A sleeping thread does almost nothing on a CPU. Ten per cent is a
	// ceiling with room to spare, not a measurement.
	if share := float64(sleeper.OnCPU) / float64(sleeper.Observed); share > 0.10 {
		t.Errorf("on-cpu was %.1f%% of the observation, want a sleeping thread near zero", 100*share)
	}
	assertDecompositionCloses(t, sleeper)
}

// The other half of the same claim: a thread that never yields should show up
// as on-CPU, not as blocked. Getting this backwards is invisible in a report
// that only says "not running".
func TestASpinningThreadIsOnCPU(t *testing.T) {
	kerneltest.Root(t)

	session, err := offcpu.Open(0)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer session.Close()

	const spin = 600 * time.Millisecond
	done := make(chan struct{})
	go func() {
		defer close(done)
		nameThisThread(t, spinnerName)
		defer runtime.UnlockOSThread()
		deadline := time.Now().Add(spin)
		for time.Now().Before(deadline) { //nolint:revive // burning CPU is the point
		}
	}()
	<-done

	threads, err := session.Threads()
	if err != nil {
		t.Fatalf("threads: %v", err)
	}
	spinner, ok := findByComm(threads, spinnerName)
	if !ok {
		t.Fatalf("no thread named %q among %d observed", spinnerName, len(threads))
	}

	// Runnable time, not on-CPU time. The machine has other work, so the
	// spinner is preempted and some of its time is spent in the runqueue --
	// which is a real measurement, not noise, and lumping it into on-CPU
	// would hide exactly the quantity this project is about. What the test
	// asserts is that it was runnable throughout rather than blocked.
	//
	// A share of what was observed, not of how long it spun. A thread that is
	// never preempted generates no scheduler events at all, so the tool first
	// sees it at its first context switch and honestly reports a shorter
	// observation than the spin -- on a quiet four-CPU runner that was 310 ms
	// of a 600 ms spin. Asserting against the spin would be asserting that the
	// machine was busy enough to preempt it, which is a fact about the machine
	// and not about the measurement.
	runnable := spinner.OnCPU + spinner.Runqueue
	if spinner.Observed <= 0 {
		t.Fatalf("the spinner was never observed at all")
	}
	if share := float64(runnable) / float64(spinner.Observed); share < 0.95 {
		t.Errorf("runnable was %.1f%% of the observation (on-cpu %v + runqueue %v of %v), "+
			"want a spinning thread to be runnable throughout",
			100*share, spinner.OnCPU, spinner.Runqueue, spinner.Observed)
	}
	if share := float64(spinner.Blocked) / float64(spinner.Observed); share > 0.10 {
		t.Errorf("blocked was %.1f%% of the observation, want a spinning thread near zero",
			100*share)
	}
	// On an unloaded machine a spinner should mostly be running, not queued.
	// This is deliberately weak: it is a statement about this machine having
	// spare CPUs, and it is here to catch the state machine filing on-CPU
	// time as runqueue delay, which would pass every assertion above.
	if spinner.OnCPU < spinner.Runqueue {
		t.Errorf("spent more time queued (%v) than running (%v) on an idle machine",
			spinner.Runqueue, spinner.OnCPU)
	}
	assertDecompositionCloses(t, spinner)
}

// The headline claim of the whole tool, asserted rather than asserted about:
// the categories account for all of the time and none of it twice.
func TestEveryDecompositionCloses(t *testing.T) {
	kerneltest.Root(t)

	session, err := offcpu.Open(0)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer session.Close()

	// Long enough for a quiet machine to schedule something.
	time.Sleep(300 * time.Millisecond)

	threads, err := session.Threads()
	if err != nil {
		t.Fatalf("threads: %v", err)
	}
	if len(threads) == 0 {
		t.Fatal("no threads were observed at all, on a machine that is running this test")
	}

	for _, thread := range threads {
		assertDecompositionCloses(t, thread)
	}
	t.Logf("%d threads, every decomposition closing to within %v", len(threads), offcpu.ReadSkewTolerance)

	drops, err := session.Drops()
	if err != nil {
		t.Fatalf("drops: %v", err)
	}
	if drops.Any() {
		t.Errorf("threads were lost during the test: %+v", drops)
	}
}

func assertDecompositionCloses(t *testing.T, thread offcpu.Thread) {
	t.Helper()
	residual := thread.Residual()
	if residual < 0 {
		residual = -residual
	}
	if residual > offcpu.ReadSkewTolerance {
		t.Errorf("thread %d (%s): observed %v but accounted %v, off by %v -- "+
			"past what reading a live map explains",
			thread.TID, thread.Comm, thread.Observed, thread.Accounted(), residual)
	}
}

// The target filter has to narrow what the kernel does, not what userspace
// prints. Passing this process's own pid is the one case a test can construct
// without knowing the initial namespace's numbering.
func TestFilteringToAProcessExcludesTheRestOfTheMachine(t *testing.T) {
	kerneltest.Root(t)

	if initial, err := inInitialNamespace(); err == nil && !initial {
		t.Skip("this process is in its own pid namespace, so /proc pids do not " +
			"mean anything to the in-kernel filter; the filter is exercised on a " +
			"host where they agree")
	}

	session, err := offcpu.Open(unix.Getpid())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer session.Close()

	done := make(chan struct{})
	go func() {
		defer close(done)
		nameThisThread(t, sleeperName)
		defer runtime.UnlockOSThread()
		time.Sleep(300 * time.Millisecond)
	}()
	<-done

	threads, err := session.Threads()
	if err != nil {
		t.Fatalf("threads: %v", err)
	}
	if len(threads) == 0 {
		t.Fatal("filtering to this process observed no threads at all")
	}
	if _, ok := findByComm(threads, sleeperName); !ok {
		t.Errorf("the filtered session did not see this process's own %q thread", sleeperName)
	}
	// Every thread of this process is either named by the test or named
	// after the test binary; anything else means the filter let the machine
	// through.
	for _, thread := range threads {
		// Threads this binary named itself, in this test or an earlier one.
		// A prefix rather than a list: the names outlive the tests that set
		// them, because the Go runtime keeps the thread and hands it to the
		// next goroutine, so a test added later would otherwise be reported
		// here as an intruder. That is exactly what happened when the
		// cross-check test introduced a third name.
		if strings.HasPrefix(thread.Comm, testThreadPrefix) {
			continue
		}
		if !isThisProcessThread(thread.Comm) {
			t.Errorf("thread %d (%s) is not part of this process, so the filter leaked",
				thread.TID, thread.Comm)
		}
	}
}

func inInitialNamespace() (bool, error) {
	var st unix.Stat_t
	if err := unix.Stat("/proc/self/ns/pid", &st); err != nil {
		return false, err
	}
	const procPIDInitIno = 0xEFFFFFFC
	return st.Ino == procPIDInitIno, nil
}

// isThisProcessThread reports whether a comm could belong to this test binary.
// Go names its runtime threads after the process, truncated to 15 characters.
func isThisProcessThread(comm string) bool { return comm == processComm }

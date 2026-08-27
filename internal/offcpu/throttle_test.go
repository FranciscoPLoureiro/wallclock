package offcpu_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/FranciscoPLoureiro/wallclock/internal/kerneltest"
	"github.com/FranciscoPLoureiro/wallclock/internal/offcpu"
	"github.com/FranciscoPLoureiro/wallclock/internal/spin"
)

// The quota this test imposes: 20 ms of CPU in every 100 ms period.
//
// Chosen so the arithmetic is not a matter of opinion. A thread that never
// stops asking for CPU can have a fifth of one, so four fifths of its wall
// clock has to be spent ready and forbidden -- and the whole point of the
// phase is that those four fifths are not the same thing as a busy machine,
// on a machine that is not busy.
const (
	quotaUsec  = 20000
	periodUsec = 100000
	quotaShare = float64(quotaUsec) / float64(periodUsec)
)

// A cgroup with a tight cpu.max, and a thread inside it that spins.
//
// This is the category no existing tool separates, measured against a number
// decided in advance rather than observed and then explained.
func TestAThrottledThreadIsNotJustQueued(t *testing.T) {
	kerneltest.Root(t)

	cgroup := newTestCgroup(t)

	session, err := offcpu.Open(0)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer session.Close()

	const (
		spin = 2 * time.Second
		name = testThreadPrefix + "throttled"
	)

	// A separate process rather than a thread of this one, for two reasons.
	// Moving a single thread into a cgroup needs the cgroup put into threaded
	// mode first, and moving this process would throttle the test harness
	// along with its subject. A child in its own cgroup is also the shape of
	// the thing being modelled: a container with a cpu limit.
	spinner := startSpinner(t, name)
	cgroup.admit(t, spinner.pid)
	time.Sleep(spin)
	spinner.stop(t)

	threads, err := session.Threads()
	if err != nil {
		t.Fatalf("threads: %v", err)
	}
	subject, ok := findByComm(threads, name)
	if !ok {
		t.Fatalf("no thread named %q among %d observed", name, len(threads))
	}

	t.Logf("observed %v: on-cpu %v, throttled %v, runqueue %v, blocked %v",
		subject.Observed, subject.OnCPU, subject.Throttled,
		subject.Runqueue, subject.Blocked)

	if subject.Throttled == 0 {
		t.Fatalf("a thread spinning in a cgroup limited to %d%% of a CPU reported "+
			"no throttled time at all", int(100*quotaShare))
	}

	// The claim under test, and it is a claim about which column the time
	// lands in rather than about the machine: nearly all of this thread's
	// wait was its own quota, not a shortage of CPUs. On an idle sixteen-core
	// machine ordinary runqueue delay should be a rounding error beside it.
	ready := subject.Throttled + subject.Runqueue
	if ready == 0 {
		t.Fatal("the thread was never ready and waiting, so nothing was measured")
	}
	if share := float64(subject.Throttled) / float64(ready); share < 0.9 {
		t.Errorf("only %.1f%% of the time this thread spent ready is attributed to "+
			"throttling (throttled %v, runqueue %v) -- on an idle machine a capped "+
			"cgroup should account for nearly all of it",
			100*share, subject.Throttled, subject.Runqueue)
	}

	// And the arithmetic. It can have quotaShare of a CPU, so about that
	// share of its observed time should be on-CPU. The tolerance is wide on
	// purpose: the thread is admitted to the cgroup a moment after it starts,
	// and the period boundary means it can be up to one full quota ahead or
	// behind at either end of a two second window.
	onCPUShare := float64(subject.OnCPU) / float64(subject.Observed)
	if onCPUShare < quotaShare/2 || onCPUShare > quotaShare*2 {
		t.Errorf("on-CPU was %.1f%% of the observation, want about %.0f%% "+
			"(the cgroup allows %dms in every %dms)",
			100*onCPUShare, 100*quotaShare, quotaUsec/1000, periodUsec/1000)
	}

	assertDecompositionCloses(t, subject)

	// The second opinion, and the one that makes this more than a tool
	// agreeing with itself: the kernel keeps its own tally of how long this
	// cgroup was throttled, by a route that shares nothing with the kprobes
	// above.
	kernelSays := cgroup.throttledUsec(t)
	if kernelSays == 0 {
		t.Fatal("cpu.stat reports the cgroup was never throttled, while wallclock " +
			"reports it was: one of the two is wrong")
	}
	t.Logf("cpu.stat: throttled_usec=%d, nr_throttled=%d", kernelSays, cgroup.nrThrottled(t))

	// These are not the same quantity and the bounds are not symmetric.
	// cpu.stat counts how long the *cgroup* was throttled; wallclock counts
	// how long *this thread* was kept waiting by that throttling. With one
	// thread in the cgroup they should very nearly coincide, and they cannot
	// exceed each other by much in either direction: the thread cannot be
	// held back longer than its group was, and on a machine with spare CPUs
	// it should not be held back much less.
	//
	// The upper bound has a little slack for the two clocks not starting at
	// the same instant. The lower bound has more, because a loaded machine
	// can leave the thread not-runnable for part of a throttled window, which
	// would be a fact about the machine rather than a disagreement.
	kernelThrottled := time.Duration(kernelSays) * time.Microsecond
	ratio := float64(subject.Throttled) / float64(kernelThrottled)
	if ratio < 0.5 || ratio > 1.1 {
		t.Errorf("wallclock reports %v throttled for this thread and cpu.stat "+
			"reports %v for its cgroup: a ratio of %.3f. Two independent accounts "+
			"of the same throttling disagree",
			subject.Throttled, kernelThrottled, ratio)
	}
	t.Logf("wallclock %v against cpu.stat %v, ratio %.4f",
		subject.Throttled, kernelThrottled, ratio)
}

// testCgroup is a cgroup v2 directory with a cpu.max, removed at the end.
type testCgroup struct{ path string }

func newTestCgroup(t *testing.T) *testCgroup {
	t.Helper()

	const root = "/sys/fs/cgroup"
	if err := enableCPUController(root); err != nil {
		t.Skipf("the cpu controller is not delegated here, so a quota cannot be "+
			"set: %v", err)
	}

	path := filepath.Join(root, fmt.Sprintf("wallclock-test-%d", os.Getpid()))
	if err := os.Mkdir(path, 0o755); err != nil && !os.IsExist(err) {
		t.Skipf("cannot create a cgroup at %s: %v", path, err)
	}
	c := &testCgroup{path: path}
	t.Cleanup(func() {
		// Threads have to leave before the directory will go, and the test's
		// subject thread has already exited by then, so this usually just
		// works. A failure here is not the test's verdict.
		if err := os.Remove(path); err != nil {
			t.Logf("removing %s: %v", path, err)
		}
	})

	quota := fmt.Sprintf("%d %d", quotaUsec, periodUsec)
	if err := os.WriteFile(filepath.Join(path, "cpu.max"), []byte(quota), 0o644); err != nil {
		t.Skipf("cannot set cpu.max on %s: %v", path, err)
	}
	return c
}

// enableCPUController delegates cpu to children of the root, without which a
// child cgroup has no cpu.max to write.
func enableCPUController(root string) error {
	controllers, err := os.ReadFile(filepath.Join(root, "cgroup.subtree_control"))
	if err != nil {
		return err
	}
	if containsWord(string(controllers), "cpu") {
		return nil
	}
	return os.WriteFile(filepath.Join(root, "cgroup.subtree_control"), []byte("+cpu"), 0o644)
}

func containsWord(s, word string) bool {
	for _, field := range splitFields(s) {
		if field == word {
			return true
		}
	}
	return false
}

func splitFields(s string) []string {
	var out []string
	start := -1
	for i := 0; i <= len(s); i++ {
		isSpace := i == len(s) || s[i] == ' ' || s[i] == '\n' || s[i] == '\t'
		switch {
		case !isSpace && start < 0:
			start = i
		case isSpace && start >= 0:
			out = append(out, s[start:i])
			start = -1
		}
	}
	return out
}

// admit moves a process into the cgroup, where its cpu.max applies to it.
func (c *testCgroup) admit(t *testing.T, pid int) {
	t.Helper()
	err := os.WriteFile(filepath.Join(c.path, "cgroup.procs"),
		[]byte(fmt.Sprint(pid)), 0o644)
	if err != nil {
		t.Fatalf("moving process %d into %s: %v", pid, c.path, err)
	}
}

// spinner is a child process that asks for CPU and never stops.
type spinner struct {
	pid  int
	stop func(*testing.T)
}

// startSpinner runs a shell loop under a binary named for the test, because
// comm comes from the executable name and that is how the subject is found
// again in the report -- `exec -a` sets argv[0], which the kernel does not
// use for this.
func startSpinner(t *testing.T, name string) *spinner {
	t.Helper()

	binary := filepath.Join(t.TempDir(), name)
	shell, _, err := spin.Shell()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(binary, shell, 0o755); err != nil { //nolint:gosec // it has to be executable
		t.Fatalf("writing the spinner binary: %v", err)
	}

	cmd := exec.Command(binary, "-c", "while :; do :; done")
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting the spinner: %v", err)
	}
	stopped := false
	stop := func(t *testing.T) {
		t.Helper()
		if stopped {
			return
		}
		stopped = true
		if err := cmd.Process.Kill(); err != nil {
			t.Errorf("killing the spinner: %v", err)
		}
		_ = cmd.Wait()
	}
	t.Cleanup(func() { stop(t) })
	return &spinner{pid: cmd.Process.Pid, stop: stop}
}

func (c *testCgroup) statField(t *testing.T, name string) uint64 {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(c.path, "cpu.stat"))
	if err != nil {
		t.Fatalf("read cpu.stat: %v", err)
	}
	for _, line := range splitLines(string(raw)) {
		fields := splitFields(line)
		if len(fields) == 2 && fields[0] == name {
			var value uint64
			if _, err := fmt.Sscanf(fields[1], "%d", &value); err != nil {
				t.Fatalf("parse %s from cpu.stat: %v", name, err)
			}
			return value
		}
	}
	t.Fatalf("cpu.stat has no %s field:\n%s", name, raw)
	return 0
}

func (c *testCgroup) throttledUsec(t *testing.T) uint64 { return c.statField(t, "throttled_usec") }
func (c *testCgroup) nrThrottled(t *testing.T) uint64   { return c.statField(t, "nr_throttled") }

func splitLines(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	return out
}

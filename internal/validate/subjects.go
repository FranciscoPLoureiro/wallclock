package validate

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/FranciscoPLoureiro/wallclock/internal/ksyms"
	"github.com/FranciscoPLoureiro/wallclock/internal/offcpu"
)

// subjectPrefix marks every thread and process this package creates, so a
// scenario can find its own subjects among everything else the machine is
// doing without keeping a list in step with the scenarios that create them.
//
// Names, not pids. The kernel numbers pids in the initial namespace and this
// process may not be in it, so a pid read here can mean a different thread
// there. A name set through /proc/thread-self/comm is the same string on both
// sides. See the pidns package.
const subjectPrefix = "wc-v-"

// observation is what one scenario saw: the subject threads, and where their
// blocked time went.
type observation struct {
	threads []offcpu.Thread
	blocked []offcpu.Blocked
}

// observe opens a profiling session, runs the workload, and returns the
// threads whose names begin with the given subject suffix.
//
// The session is opened before the workload starts and read after it
// finishes, so the window covers the whole of it. A thread is only ever
// observed from its first scheduler event, so a subject that is created after
// the session opens has its entire life inside the window -- which is what
// makes the expected values arithmetic rather than estimates.
func observe(suffix string, withReasons bool, workload func() error) (*observation, error) {
	session, err := offcpu.Open(0)
	if err != nil {
		return nil, fmt.Errorf("open a profiling session: %w", err)
	}
	defer session.Close()

	if err := workload(); err != nil {
		return nil, err
	}

	obs := &observation{}
	var symbols *ksyms.Table
	if withReasons {
		if symbols, err = ksyms.Load(); err != nil {
			return nil, fmt.Errorf("naming kernel stacks: %w", err)
		}
		// Reasons before totals, then open waits: the other order counts an
		// interval that ends between the two reads twice. See BlockedReasons.
		if obs.blocked, err = session.BlockedReasons(symbols); err != nil {
			return nil, err
		}
	}

	all, err := session.Threads()
	if err != nil {
		return nil, err
	}
	if withReasons {
		if obs.blocked, err = session.AttributeOpenWaits(obs.blocked, all, symbols); err != nil {
			return nil, err
		}
	}

	want := subjectPrefix + suffix
	for _, t := range all {
		if strings.HasPrefix(t.Comm, want) {
			obs.threads = append(obs.threads, t)
		}
	}
	if len(obs.threads) == 0 {
		return nil, fmt.Errorf("no thread named %q* was observed among %d: the "+
			"subject never reached a scheduler event", want, len(all))
	}
	return obs, nil
}

// sum adds one field over every subject thread.
func (o *observation) sum(field func(offcpu.Thread) time.Duration) time.Duration {
	var total time.Duration
	for _, t := range o.threads {
		total += field(t)
	}
	return total
}

// nameThisThread pins the calling goroutine to its OS thread and names that
// thread.
//
// The pinning is not decoration. Without it the runtime may move the
// goroutine mid-scenario, and the measurement would then be of two threads,
// neither of them completely. Written through /proc/thread-self/comm rather
// than prctl: thread-self is the calling thread by definition, so it needs no
// pid at all -- convenient here, where pids are the thing that cannot be
// trusted to mean the same on both sides.
func nameThisThread(suffix string) (func(), error) {
	runtime.LockOSThread()
	name := subjectPrefix + suffix
	// The mode is ignored: this is an existing procfs file whose permissions
	// the kernel owns, not a file being created.
	if err := os.WriteFile("/proc/thread-self/comm", []byte(name), 0o644); err != nil { //nolint:gosec // see above
		runtime.UnlockOSThread()
		return nil, fmt.Errorf("naming this thread %q: %w", name, err)
	}
	return runtime.UnlockOSThread, nil
}

// spinners is a set of child processes that ask for CPU and never stop.
type spinners struct {
	cmds []*exec.Cmd
}

// startSpinners runs n copies of a shell loop under a binary named for the
// scenario.
//
// A copied binary rather than `exec -a`, because comm comes from the
// executable name and the kernel does not read argv[0] for it. Child
// processes rather than threads of this one, because a scenario that wants to
// saturate the machine must not saturate the profiler reading the result, and
// because a cgroup limit is a thing you put a process into.
func startSpinners(n int, suffix string) (*spinners, error) {
	dir, err := os.MkdirTemp("", "wallclock-validate")
	if err != nil {
		return nil, fmt.Errorf("temporary directory for the spinner: %w", err)
	}
	shell, err := os.ReadFile("/bin/dash")
	if err != nil {
		return nil, fmt.Errorf("reading /bin/dash to copy as the spinner: %w", err)
	}
	binary := filepath.Join(dir, subjectPrefix+suffix)
	if err := os.WriteFile(binary, shell, 0o755); err != nil { //nolint:gosec // it has to be executable
		return nil, fmt.Errorf("writing the spinner binary: %w", err)
	}

	s := &spinners{}
	for range n {
		cmd := exec.Command(binary, "-c", "while :; do :; done")
		if err := cmd.Start(); err != nil {
			s.stop()
			return nil, fmt.Errorf("starting a spinner: %w", err)
		}
		s.cmds = append(s.cmds, cmd)
	}
	return s, nil
}

// pids reports the spinners' process ids, as this process's namespace numbers
// them -- which is what cgroup.procs wants and is not what the kernel reports
// to BPF.
func (s *spinners) pids() []int {
	out := make([]int, 0, len(s.cmds))
	for _, cmd := range s.cmds {
		out = append(out, cmd.Process.Pid)
	}
	return out
}

func (s *spinners) stop() {
	for _, cmd := range s.cmds {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	}
	s.cmds = nil
}

// cgroup is a directory under the cgroup2 root with a cpu quota on it.
type cgroup struct {
	path string
}

// newCgroup creates a cgroup allowed quota microseconds of CPU in every
// period microseconds.
func newCgroup(quotaUsec, periodUsec int) (*cgroup, error) {
	const root = "/sys/fs/cgroup"
	if err := enableCPUController(root); err != nil {
		return nil, fmt.Errorf("delegating the cpu controller at %s: %w", root, err)
	}

	path := filepath.Join(root, fmt.Sprintf("wallclock-validate-%d", os.Getpid()))
	// 0755 because a cgroup directory has to be traversable to be usable,
	// and because the kernel decides the mode of everything inside it
	// regardless of what is asked for here.
	if err := os.Mkdir(path, 0o755); err != nil && !os.IsExist(err) { //nolint:gosec // see above
		return nil, fmt.Errorf("creating the cgroup %s: %w", path, err)
	}
	c := &cgroup{path: path}

	quota := fmt.Sprintf("%d %d", quotaUsec, periodUsec)
	// Again an existing kernel file: the mode argument goes nowhere.
	if err := os.WriteFile(filepath.Join(path, "cpu.max"), []byte(quota), 0o644); err != nil { //nolint:gosec // see above
		_ = c.remove()
		return nil, fmt.Errorf("setting cpu.max on %s: %w", path, err)
	}
	return c, nil
}

// enableCPUController delegates cpu to children of the root, without which a
// child cgroup has no cpu.max to write.
func enableCPUController(root string) error {
	controllers, err := os.ReadFile(filepath.Join(root, "cgroup.subtree_control"))
	if err != nil {
		return err
	}
	for _, field := range strings.Fields(string(controllers)) {
		if field == "cpu" {
			return nil
		}
	}
	return os.WriteFile(filepath.Join(root, "cgroup.subtree_control"), []byte("+cpu"), 0o644) //nolint:gosec // an existing kernel file; the mode is ignored
}

// admit moves a process into the cgroup, where its cpu.max applies to it.
func (c *cgroup) admit(pid int) error {
	err := os.WriteFile(filepath.Join(c.path, "cgroup.procs"), []byte(fmt.Sprint(pid)), 0o644) //nolint:gosec // an existing kernel file; the mode is ignored
	if err != nil {
		return fmt.Errorf("moving process %d into %s: %w", pid, c.path, err)
	}
	return nil
}

// throttledUsec reads the kernel's own account of how long this cgroup was
// held off a CPU -- the second opinion the throttling scenario is checked
// against.
func (c *cgroup) throttledUsec() (uint64, error) {
	raw, err := os.ReadFile(filepath.Join(c.path, "cpu.stat"))
	if err != nil {
		return 0, fmt.Errorf("read cpu.stat: %w", err)
	}
	for _, line := range strings.Split(string(raw), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[0] == "throttled_usec" {
			var value uint64
			if _, err := fmt.Sscanf(fields[1], "%d", &value); err != nil {
				return 0, fmt.Errorf("parse throttled_usec: %w", err)
			}
			return value, nil
		}
	}
	return 0, errors.New("cpu.stat has no throttled_usec field")
}

// remove takes the cgroup away. Its processes have to be gone first, which
// they are by the time a scenario is finished with them.
func (c *cgroup) remove() error {
	if err := os.Remove(c.path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("removing %s: %w", c.path, err)
	}
	return nil
}

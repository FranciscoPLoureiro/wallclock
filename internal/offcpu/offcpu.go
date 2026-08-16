// Package offcpu decomposes a thread's wall clock into the states the
// scheduler moved it through: on a CPU, ready and waiting for one, or blocked
// waiting for something else.
//
// The distinction between the last two is the reason this exists. An off-CPU
// profiler reports "not running" as one quantity, and the two halves of it
// have opposite remedies: a thread blocked on a socket is waiting for a
// reply, and a thread sitting in the runqueue already has its reply and is
// waiting for a CPU. Telling a team to buy a bigger machine when the answer
// was a slow query -- or the reverse -- is what that conflation costs.
package offcpu

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"
	"github.com/cilium/ebpf/rlimit"
	"golang.org/x/sys/unix"

	"github.com/FranciscoPLoureiro/wallclock/internal/pidns"
)

//go:generate go tool bpf2go -target bpfel -type thread -type stat_slot offcpu ../../bpf/offcpu.bpf.c -- -D__TARGET_ARCH_x86 -I/usr/include/x86_64-linux-gnu

// State is what a thread was doing.
type State uint32

// The states a thread moves between, matching the enum in the BPF program.
const (
	StateUnknown  State = 0
	StateOnCPU    State = 1
	StateRunqueue State = 2
	StateBlocked  State = 3
	StateExited   State = 4
)

// Thread is one thread's decomposition over the time it was observed.
type Thread struct {
	// TID is the thread id **as the initial pid namespace numbers it**,
	// which inside a container is not the number /proc shows. See the pidns
	// package.
	TID uint32
	// Exited reports that the thread was gone by the time this was read, so
	// its numbers are final and its observation window closed when it died
	// rather than when the report was produced. Short-lived work is real
	// work and it is reported; what it must not do is go on accruing
	// blocked time after the thread that earned it has stopped existing.
	Exited bool
	// Comm is the thread's name, taken from the tracepoint rather than from
	// /proc, so it is right for threads that have since exited and for pids
	// this process cannot look up.
	Comm string

	// Observed is the wall clock this decomposition covers: the time from
	// when the thread was first seen to when it was read. It is not the
	// session window. A thread first seen halfway through has half a window
	// nobody watched, and reporting percentages of the session would quietly
	// attribute time that was never measured.
	Observed time.Duration

	OnCPU time.Duration
	// Runqueue is time spent ready with no CPU free to run on.
	Runqueue time.Duration
	// Throttled is time spent ready on a machine that had a CPU free, with
	// the cgroup's own quota forbidding its use.
	//
	// Both are a thread that is ready and not running, and from outside they
	// are the same thing. They are opposite problems: the first says buy a
	// bigger machine, the second says raise a limit -- and a tool that
	// reports them as one number is how a team buys hardware it did not
	// need. Separating them is the reason this project exists.
	Throttled time.Duration
	Blocked   time.Duration
	// CgroupID is the cgroup the thread was last seen in, as the kernel
	// numbers it, or zero if it has not been observed leaving a CPU yet.
	// Unlike a pid this means the same thing inside a container and out.
	CgroupID uint64
	// Unknown is time the thread spent in a state this tool could not name.
	// It should be zero, and it is accumulated in the kernel rather than
	// merely displayed, so that it can actually become non-zero: a state
	// added without a matching case would otherwise lose its time silently
	// while this column went on reporting zero and reading like assurance.
	Unknown time.Duration
}

// ReadSkewTolerance is how far a decomposition may fail to close before it is
// a defect rather than the cost of measuring.
//
// The map is read while the kernel writes to it, so a thread can change state
// between the clock read and the map read for its entry. That gap is bounded
// by one vDSO call and one map lookup. Anything beyond this is not skew: it
// means a transition credited time to no category, and the state machine is
// missing a case.
const ReadSkewTolerance = 1 * time.Millisecond

// Accounted is the sum of the categories, which by construction equals
// Observed. It is computed rather than assumed so that the report can show
// the two side by side and the reader can see the books balance.
func (t Thread) Accounted() time.Duration {
	return t.OnCPU + t.Runqueue + t.Throttled + t.Blocked + t.Unknown
}

// Residual is what the categories failed to account for. Zero means the
// decomposition closes.
func (t Thread) Residual() time.Duration { return t.Observed - t.Accounted() }

// Drops is what the kernel side could not record.
type Drops struct {
	// EventsDropped counts scheduler *events* discarded because the threads
	// map was at max_entries -- not the threads behind them. When the map is
	// full the insert fails, the entry never comes into existence, and the
	// next event for the same thread takes the same path, so one untracked
	// thread contributes an event per context switch. Reporting this as a
	// count of threads printed "LOST 50000 threads entirely" on machines
	// with a few hundred, which is the kind of number that destroys trust in
	// the lines around it.
	EventsDropped uint64
	// TargetsFull counts new threads of a tracked process that could not be
	// added to the target set, so their time was never collected. This one
	// really is a thread count: there is one fork per thread.
	TargetsFull uint64
	// CgroupsFull counts cgroups whose throttling could not be recorded
	// because that map was at max_entries. Their threads' waits are then
	// filed as ordinary runqueue delay -- the two categories this phase
	// exists to separate, silently merged again -- so it is reported rather
	// than absorbed.
	CgroupsFull uint64
}

// Any reports whether anything was lost.
func (d Drops) Any() bool {
	return d.EventsDropped > 0 || d.TargetsFull > 0 || d.CgroupsFull > 0
}

// Session owns the loaded programs, their maps and the attachments.
type Session struct {
	objs  offcpuObjects
	links []link.Link
}

// Open loads the programs and attaches them to the scheduler tracepoints.
//
// A zero target follows every thread on the machine, which is what runqlat
// and friends do. Naming a pid narrows it in the kernel: userspace seeds the
// target set from /proc/<pid>/task and the fork program keeps it current, so
// the tracepoints do almost nothing for the threads nobody asked about.
func Open(targetPID int) (_ *Session, err error) {
	if err := rlimit.RemoveMemlock(); err != nil {
		return nil, fmt.Errorf("raise RLIMIT_MEMLOCK: %w", err)
	}
	raiseMemlockForPerfEvents()

	spec, err := loadOffcpu()
	if err != nil {
		return nil, fmt.Errorf("read the embedded object: %w", err)
	}

	filtering := uint8(0)
	if targetPID > 0 {
		filtering = 1
	}
	if err := spec.Variables["filter_targets"].Set(filtering); err != nil {
		return nil, fmt.Errorf("set filter_targets: %w", err)
	}

	s := &Session{}
	if err := spec.LoadAndAssign(&s.objs, nil); err != nil {
		// Wrapped, so a caller can still errors.As its way to the verifier
		// error; bpfload.Explain is what renders the annotated log, which
		// only appears under %+v.
		return nil, fmt.Errorf("load: %w", err)
	}
	defer func() {
		if err != nil {
			_ = s.Close()
		}
	}()

	if targetPID > 0 {
		if err = s.seedTargets(targetPID); err != nil {
			return nil, err
		}
	}

	// Attached after the targets are seeded. The other order leaves a window
	// in which the programs run against an empty target set and file the
	// process as untracked -- briefly, silently, and only under load.
	//
	// Exit goes on first, and the order is not cosmetic. Every other program
	// can bring an entry into existence, and exit is the only one that can
	// close it, so any gap between them is a window in which a thread can be
	// recorded and then die unobserved -- leaving an entry that never wakes
	// and accrues blocked time for the rest of the session. It is a window of
	// microseconds and it landed every time, because the threads exiting just
	// then are the ones the profiler's own startup is racing: a /bin/true and
	// a /bin/echo, at the top of the report, blocked for the whole window.
	//
	// These two are raw tracepoints, given the task_struct pointers the
	// kernel passes internally and reading through CO-RE, which is what makes
	// them work on a kernel whose formatted layout is not the one they were
	// compiled against. See the comment in bpf/offcpu.bpf.c.
	for _, raw := range []struct {
		name    string
		program *ebpf.Program
	}{
		{"sched_process_exit", s.objs.OnSchedProcessExit},
		{"sched_process_fork", s.objs.OnSchedProcessFork},
	} {
		l, err := link.AttachRawTracepoint(link.RawTracepointOptions{
			Name:    raw.name,
			Program: raw.program,
		})
		if err != nil {
			return nil, fmt.Errorf("attach to the raw %s tracepoint: %w", raw.name, err)
		}
		s.links = append(s.links, l)
	}

	// The throttling probes go on before the scheduler tracepoints, for the
	// same reason exit does: a thread joining the runqueue reads the
	// cgroup's throttled total as its baseline, and a baseline taken before
	// these are watching reads zero for a cgroup that is already throttled.
	// The wait would then be filed as ordinary runqueue delay -- the exact
	// confusion this phase exists to remove.
	for _, probe := range []struct {
		symbol  string
		program *ebpf.Program
	}{
		{"throttle_cfs_rq", s.objs.OnThrottleCfsRq},
		{"unthrottle_cfs_rq", s.objs.OnUnthrottleCfsRq},
	} {
		l, err := link.Kprobe(probe.symbol, probe.program, nil)
		if err != nil {
			return nil, fmt.Errorf("attach a kprobe to %s: %w", probe.symbol, err)
		}
		s.links = append(s.links, l)
	}

	for _, attachment := range []struct {
		name    string
		program *ebpf.Program
	}{
		{"sched_switch", s.objs.OnSchedSwitch},
		{"sched_wakeup", s.objs.OnSchedWakeup},
		{"sched_wakeup_new", s.objs.OnSchedWakeupNew},
	} {
		l, err := link.Tracepoint("sched", attachment.name, attachment.program, nil)
		if err != nil {
			return nil, fmt.Errorf("attach to sched:%s: %w", attachment.name, err)
		}
		s.links = append(s.links, l)
	}

	// Seeded once more, now that the fork program is watching. The first
	// pass ran before anything was attached, so a thread the target started
	// in between appears in neither -- not in the snapshot, because it did
	// not exist yet, and not through the fork program, because nothing was
	// listening. Nothing re-seeds later, so that thread would stay missing
	// for the whole session with no counter to show it: the process would
	// simply appear to do less work than it does. A second /proc walk closes
	// the window.
	if targetPID > 0 {
		if err = s.seedTargets(targetPID); err != nil {
			return nil, err
		}
	}

	return s, nil
}

// seedTargets fills the target set from the threads a process has right now.
//
// The pids come from /proc, which numbers them in *this* process's namespace,
// while the kernel filters on the initial namespace's numbering. Where those
// differ -- any container, and the WSL distribution this was developed in --
// the two disagree and the filter matches nothing. That is checked and
// refused rather than left to produce an empty report that looks like an idle
// process.
func (s *Session) seedTargets(pid int) error {
	// Checked, not assumed, and refused rather than warned about. Where the
	// namespaces differ these two numberings disagree for every thread, the
	// filter matches nothing at all, and the report that comes out is an
	// empty one -- which reads exactly like a process that was idle. There
	// is no partial success to salvage here, so failing is the only honest
	// outcome.
	initial, err := pidns.InInitial()
	if err != nil {
		return fmt.Errorf("determine this process's pid namespace: %w", err)
	}
	if !initial {
		return fmt.Errorf(
			"cannot filter by pid from inside a pid namespace: the kernel reports "+
				"pids as the initial namespace numbers them, and pid %d here means "+
				"a different thread there. Run wallclock in the initial namespace, "+
				"or profile without -pid", pid)
	}

	taskDir := filepath.Join("/proc", strconv.Itoa(pid), "task")
	entries, err := os.ReadDir(taskDir)
	if err != nil {
		return fmt.Errorf("list the threads of pid %d: %w", pid, err)
	}

	yes := uint8(1)
	seeded := 0
	for _, entry := range entries {
		tid, err := strconv.ParseUint(entry.Name(), 10, 32)
		if err != nil {
			continue
		}
		if err := s.objs.Targets.Put(uint32(tid), yes); err != nil {
			return fmt.Errorf("add thread %d to the target set: %w", tid, err)
		}
		seeded++
	}
	if seeded == 0 {
		return fmt.Errorf("pid %d has no threads in %s", pid, taskDir)
	}
	return nil
}

// Threads reads the decomposition for every thread observed so far.
//
// The time each thread has spent in its *current* state is added here rather
// than in the kernel, because nothing in the kernel runs at read time. Without
// it, a thread that blocked eight seconds ago and has not woken since reports
// eight seconds of nothing at all, and the categories stop summing to the
// time observed.
//
// The clock is read once per entry rather than once for the whole map. The
// kernel keeps writing while this iterates, so a timestamp taken at the top
// is already stale by the time the last thread is read, and the difference
// lands in Residual as an apparent hole in the state machine. Reading it per
// entry shrinks that to the cost of one vDSO call, which is what the residual
// then measures: the price of reading a live map, not an accounting error.
func (s *Session) Threads() ([]Thread, error) {
	var (
		out []Thread
		tid uint32
		raw offcpuThread
	)
	// Exited entries are read once and then removed. Draining them here is
	// what bounds the map: without it every short-lived process on the
	// machine occupies a slot until the session ends, and on anything with
	// process churn the map reaches max_entries and stops tracking anybody
	// new. Collected during the walk and deleted after it, because removing
	// the key an iterator is standing on is not something to rely on.
	var drain []uint32

	iter := s.objs.Threads.Iterate()
	for iter.Next(&tid, &raw) {
		now, err := monotonicNow()
		if err != nil {
			return nil, err
		}
		t := Thread{
			TID:       raw.Tid,
			Comm:      commToString(raw.Comm),
			CgroupID:  raw.CgroupId,
			OnCPU:     time.Duration(raw.OnCpuNs),     //nolint:gosec // ns since boot, signed after 292 years
			Runqueue:  time.Duration(raw.RunqueueNs),  //nolint:gosec // as above
			Throttled: time.Duration(raw.ThrottledNs), //nolint:gosec // as above
			Blocked:   time.Duration(raw.BlockedNs),   //nolint:gosec // as above
			Unknown:   time.Duration(raw.UnknownNs),   //nolint:gosec // as above
		}

		if State(raw.State) == StateExited {
			// Its clock stopped when it died. SinceNs is the moment of
			// exit, so the observation window closed there and nothing
			// is added for the time since.
			t.Exited = true
			if raw.SinceNs > raw.FirstSeenNs {
				t.Observed = time.Duration(raw.SinceNs - raw.FirstSeenNs) //nolint:gosec // as above
			}
			drain = append(drain, tid)
			out = append(out, t)
			continue
		}

		// Close out the state the thread is in right now.
		if now > raw.SinceNs {
			current := time.Duration(now - raw.SinceNs) //nolint:gosec // as above
			switch State(raw.State) {
			case StateOnCPU:
				t.OnCPU += current
			case StateRunqueue:
				// Credited whole to runqueue rather than split. The
				// kernel does the splitting when the wait ends, and
				// this wait has not ended: the thread is still queued
				// as the report is being written. Splitting it here
				// would mean reimplementing the same arithmetic in a
				// second place against a snapshot userspace cannot see.
				t.Runqueue += current
			case StateBlocked:
				t.Blocked += current
			case StateUnknown, StateExited:
				t.Unknown += current
			}
		}
		if now > raw.FirstSeenNs {
			t.Observed = time.Duration(now - raw.FirstSeenNs) //nolint:gosec // as above
		}
		out = append(out, t)
	}
	if err := iter.Err(); err != nil {
		return nil, fmt.Errorf("iterate the threads map: %w", err)
	}

	for _, dead := range drain {
		if err := s.objs.Threads.Delete(dead); err != nil {
			return nil, fmt.Errorf("remove exited thread %d: %w", dead, err)
		}
	}
	return out, nil
}

// Drops reports what the kernel side could not record.
func (s *Session) Drops() (Drops, error) {
	var d Drops
	for _, slot := range []struct {
		index offcpuStatSlot
		into  *uint64
	}{
		{offcpuStatSlotSTAT_EVENTS_DROPPED, &d.EventsDropped},
		{offcpuStatSlotSTAT_TARGETS_FULL, &d.TargetsFull},
		{offcpuStatSlotSTAT_CGROUPS_FULL, &d.CgroupsFull},
	} {
		if err := s.objs.Stats.Lookup(uint32(slot.index), slot.into); err != nil {
			return Drops{}, fmt.Errorf("read drop counter %d: %w", slot.index, err)
		}
	}
	return d, nil
}

// Close detaches every program and releases the maps.
func (s *Session) Close() error {
	var errs []error
	for _, l := range s.links {
		errs = append(errs, l.Close())
	}
	s.links = nil
	errs = append(errs, s.objs.Close())
	return errors.Join(errs...)
}

// raiseMemlockForPerfEvents lifts RLIMIT_MEMLOCK, which attaching to a
// tracepoint still needs even on a kernel where loading BPF does not.
//
// cilium/ebpf's rlimit.RemoveMemlock covers BPF maps and programs, and from
// 5.11 those are charged to the memory cgroup instead, so on a modern kernel
// it correctly does nothing. Perf events did not move: their ring buffers are
// still charged against RLIMIT_MEMLOCK and perf_event_mlock_kb, and every
// tracepoint attachment is a perf event.
//
// The symptom of not doing this was a session that attached three programs
// and was refused the fourth with EPERM -- only under sudo, which applies the
// PAM limits, and never when run as root directly, which does not. That is a
// difference between CI and a development machine, so it fails in the place
// least convenient to debug.
//
// Best effort on purpose. It needs CAP_SYS_RESOURCE, which anything able to
// load these programs already has, and where the limit is already generous
// there is nothing to do.
func raiseMemlockForPerfEvents() {
	unlimited := unix.Rlimit{Cur: unix.RLIM_INFINITY, Max: unix.RLIM_INFINITY}
	_ = unix.Setrlimit(unix.RLIMIT_MEMLOCK, &unlimited)
}

// monotonicNow reads the same clock bpf_ktime_get_ns reads.
//
// Not time.Now: that is the wall clock, it steps when NTP corrects it, and
// subtracting a kernel monotonic timestamp from it produces a duration that
// is wrong by however far the two clocks are apart -- which on a fresh boot
// is the entire uptime.
func monotonicNow() (uint64, error) {
	var ts unix.Timespec
	if err := unix.ClockGettime(unix.CLOCK_MONOTONIC, &ts); err != nil {
		return 0, fmt.Errorf("read CLOCK_MONOTONIC: %w", err)
	}
	return uint64(ts.Sec)*uint64(time.Second) + uint64(ts.Nsec), nil //nolint:gosec // both are non-negative
}

func commToString(comm [16]int8) string {
	b := make([]byte, 0, len(comm))
	for _, c := range comm {
		if c == 0 {
			break
		}
		b = append(b, byte(c)) //nolint:gosec // reinterpreting the kernel's char array
	}
	return string(b)
}

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
)

// Thread is one thread's decomposition over the time it was observed.
type Thread struct {
	// TID is the thread id **as the initial pid namespace numbers it**,
	// which inside a container is not the number /proc shows. See
	// syscount.InInitialPIDNamespace.
	TID uint32
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

	OnCPU    time.Duration
	Runqueue time.Duration
	Blocked  time.Duration
	// Unknown is time the thread spent in a state this tool could not name.
	// It should be zero, it is displayed anyway, and the day it is not zero
	// is the day the state machine has a hole in it.
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
	return t.OnCPU + t.Runqueue + t.Blocked + t.Unknown
}

// Residual is what the categories failed to account for. Zero means the
// decomposition closes.
func (t Thread) Residual() time.Duration { return t.Observed - t.Accounted() }

// Drops is what the kernel side could not record.
type Drops struct {
	// ThreadsFull counts threads that could not be tracked because the map
	// was at max_entries. Their time is missing entirely rather than
	// misfiled, so a non-zero value here means the report is incomplete and
	// says so.
	ThreadsFull uint64
	// TargetsFull counts new threads of a tracked process that could not be
	// added to the target set, so their time was never collected.
	TargetsFull uint64
}

// Any reports whether anything was lost.
func (d Drops) Any() bool { return d.ThreadsFull > 0 || d.TargetsFull > 0 }

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
	for _, attachment := range []struct {
		name    string
		program *ebpf.Program
	}{
		{"sched_switch", s.objs.OnSchedSwitch},
		{"sched_wakeup", s.objs.OnSchedWakeup},
		{"sched_wakeup_new", s.objs.OnSchedWakeupNew},
		{"sched_process_fork", s.objs.OnSchedProcessFork},
	} {
		l, err := link.Tracepoint("sched", attachment.name, attachment.program, nil)
		if err != nil {
			return nil, fmt.Errorf("attach to sched:%s: %w", attachment.name, err)
		}
		s.links = append(s.links, l)
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
	iter := s.objs.Threads.Iterate()
	for iter.Next(&tid, &raw) {
		now, err := monotonicNow()
		if err != nil {
			return nil, err
		}
		t := Thread{
			TID:      raw.Tid,
			Comm:     commToString(raw.Comm),
			OnCPU:    time.Duration(raw.OnCpuNs),    //nolint:gosec // ns since boot, signed after 292 years
			Runqueue: time.Duration(raw.RunqueueNs), //nolint:gosec // as above
			Blocked:  time.Duration(raw.BlockedNs),  //nolint:gosec // as above
		}

		// Close out the state the thread is in right now.
		if now > raw.SinceNs {
			current := time.Duration(now - raw.SinceNs) //nolint:gosec // as above
			switch State(raw.State) {
			case StateOnCPU:
				t.OnCPU += current
			case StateRunqueue:
				t.Runqueue += current
			case StateBlocked:
				t.Blocked += current
			case StateUnknown:
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
	return out, nil
}

// Drops reports what the kernel side could not record.
func (s *Session) Drops() (Drops, error) {
	var d Drops
	for _, slot := range []struct {
		index offcpuStatSlot
		into  *uint64
	}{
		{offcpuStatSlotSTAT_THREADS_FULL, &d.ThreadsFull},
		{offcpuStatSlotSTAT_TARGETS_FULL, &d.TargetsFull},
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

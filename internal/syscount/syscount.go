// Package syscount counts syscall entries per process, either by aggregating
// in a kernel map or by streaming every event through a ring buffer.
//
// Counting syscalls is not the point. Crossing the whole pipeline once with
// the simplest possible problem is: compile C to BPF, load it past the
// verifier, attach it to a tracepoint, filter inside the kernel, and get
// numbers back out. Phase 2 attaches to the scheduler, where the difficulty
// is the subject rather than the tools.
//
// Both transports are implemented so they can be compared rather than
// guessed at. See the README for what the comparison found.
package syscount

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/FranciscoPLoureiro/wallclock/internal/tracefs"
	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"
	"github.com/cilium/ebpf/ringbuf"
	"github.com/cilium/ebpf/rlimit"
)

//go:generate go tool bpf2go -target bpfel -type event -type count -type stat_slot syscount ../../bpf/syscount.bpf.c -- -D__TARGET_ARCH_x86 -I/usr/include/x86_64-linux-gnu

// Mode selects which of the two programs is attached. Both read the same
// tracepoint and apply the same filter; they differ in what they do with the
// event.
type Mode int

const (
	// ModeCount keeps a running total per process in a kernel hash map, read
	// on demand. One atomic add per event, no userspace work until asked.
	ModeCount Mode = iota
	// ModeStream sends every event through a ring buffer. Exact and ordered,
	// at the cost of a reservation, a copy and a wakeup per event -- and of
	// dropping events when the consumer cannot keep up.
	ModeStream
)

// Filter narrows what the kernel counts. Its zero value counts everything,
// which is why every field means "no restriction" when unset.
type Filter struct {
	// TGID limits counting to one process, by the number ps calls a PID.
	TGID uint32
	// CgroupID limits counting to one cgroup. Not a path: the kernel's
	// 64-bit cgroup id, which CgroupIDOf resolves from a path.
	CgroupID uint64
	// Syscall limits counting to one syscall number. Nil counts every
	// syscall -- a pointer rather than a sentinel because syscall 0 is a
	// real syscall (read on x86-64), and a struct literal that omitted this
	// field would otherwise silently select it.
	Syscall *int64
}

// OnlySyscall returns a Filter.Syscall restricted to one syscall number.
func OnlySyscall(nr int64) *int64 { return &nr }

// Event is one syscall entry, as seen from the kernel.
type Event struct {
	// SinceBoot is the kernel's monotonic clock at the moment of the entry.
	// It is not wall-clock time and does not survive a reboot; it is here to
	// order events and measure gaps between them.
	SinceBoot time.Duration
	// TGID is the process, PID the individual thread. The kernel calls the
	// thread a pid, which is the opposite of what userspace means by it, and
	// telling them apart matters for a tool whose subject is threads.
	TGID uint32
	PID  uint32
	// Syscall is the architecture's syscall number, not a name. Mapping
	// numbers to names is a userspace concern and an architecture-specific
	// table, so it is deliberately not done in the kernel.
	Syscall int64
	// Comm is the 16-byte name the kernel keeps for the thread.
	Comm string
}

// Drops is what the kernel side threw away. A tool that loses events in
// silence reports wrong numbers with total confidence, so these are read and
// reported alongside every result rather than on request.
type Drops struct {
	// RingbufFull counts events discarded because the ring buffer had no
	// room, meaning userspace was not draining it fast enough.
	RingbufFull uint64
	// CountsFull counts processes that could not be inserted because the
	// hash map was at max_entries. Their syscalls are missing entirely, not
	// merely undercounted.
	CountsFull uint64
}

// Any reports whether anything was lost.
func (d Drops) Any() bool { return d.RingbufFull > 0 || d.CountsFull > 0 }

// Session owns the loaded programs, their maps and the attachment.
type Session struct {
	objs   syscountObjects
	link   link.Link
	reader *ringbuf.Reader

	// Both teardown paths are routinely called from a different goroutine
	// than the one blocked in ReadEvent, and often more than once: a timer, a
	// deferred cleanup, and the reader itself on the way out. Once makes that
	// safe without a lock around every read.
	stopOnce  sync.Once
	closeOnce sync.Once
	closeErr  error
}

// Open loads the programs with the filter baked into their read-only data,
// then attaches the one the mode selects.
//
// The filter is applied by patching constants before the load, not by passing
// values at run time. The verifier sees each filter as a known value and
// folds the branch away when it is off, so an unfiltered session pays nothing
// for the filters it is not using.
func Open(mode Mode, filter Filter) (_ *Session, err error) {
	// Needed only below 5.11; a no-op above it. See docs/COMPATIBILITY.md.
	if err := rlimit.RemoveMemlock(); err != nil {
		return nil, fmt.Errorf("raise RLIMIT_MEMLOCK: %w", err)
	}

	spec, err := loadSyscount()
	if err != nil {
		return nil, fmt.Errorf("read the embedded object: %w", err)
	}

	constants := map[string]any{
		"target_tgid":   filter.TGID,
		"target_cgroup": filter.CgroupID,
	}
	if filter.Syscall != nil {
		constants["target_syscall"] = *filter.Syscall
	}
	for name, value := range constants {
		if err := spec.Variables[name].Set(value); err != nil {
			return nil, fmt.Errorf("set %s: %w", name, err)
		}
	}

	// Asked of this kernel rather than assumed from the one the object was
	// compiled against. Where the common header of a tracepoint record has
	// grown -- which is what Red Hat's PREEMPT_LAZY backport does to a kernel
	// still numbered 5.14 -- id sits four bytes further along, and a program
	// reading the old offset matches no syscall filter at all while attaching
	// and running perfectly.
	if err := tracefs.Bind(spec.Variables, "raw_syscalls", "sys_enter", []tracefs.Binding{
		{Variable: "off_sys_enter_id", Field: "id", Size: 8},
	}); err != nil {
		return nil, err
	}

	s := &Session{}
	if err := spec.LoadAndAssign(&s.objs, nil); err != nil {
		return nil, fmt.Errorf("load: %s", explainVerifier(err))
	}
	defer func() {
		if err != nil {
			// Discarded deliberately: the caller is about to be handed the
			// reason this failed, and a cleanup error reported on top of it
			// would replace the useful message with a consequence of it.
			_ = s.Close()
		}
	}()

	program := s.objs.CountSyscalls
	if mode == ModeStream {
		program = s.objs.StreamSyscalls
		if s.reader, err = ringbuf.NewReader(s.objs.Events); err != nil {
			return nil, fmt.Errorf("open the ring buffer: %w", err)
		}
	}

	// raw_syscalls:sys_enter fires once for every syscall on the machine,
	// which is what makes the in-kernel filter worth having.
	if s.link, err = link.Tracepoint("raw_syscalls", "sys_enter", program, nil); err != nil {
		return nil, fmt.Errorf("attach to raw_syscalls:sys_enter: %w", err)
	}
	return s, nil
}

// Count is what one process accumulated.
type Count struct {
	// Entries is the number of syscall entries seen.
	Entries uint64
	// Comm is the name the process had when it was first seen. It comes from
	// the kernel rather than from /proc because the pid it belongs to is in
	// the initial namespace; see the pidns package.
	Comm string
}

// Counts returns the entries seen per process since the session opened, keyed
// by pid **in the initial namespace**.
//
// The map is read while the programs keep writing to it, so the result is a
// smear across the time the read took rather than an instant. At the rate
// sys_enter fires that is unavoidable without stopping the world, and the
// alternative -- detaching to read -- loses the events that arrive meanwhile.
func (s *Session) Counts() (map[uint32]Count, error) {
	out := make(map[uint32]Count)
	var (
		tgid  uint32
		value syscountCount
	)
	iter := s.objs.Counts.Iterate()
	for iter.Next(&tgid, &value) {
		out[tgid] = Count{
			Entries: value.Entries,
			Comm:    commToString(value.Comm),
		}
	}
	if err := iter.Err(); err != nil {
		return nil, fmt.Errorf("iterate the counts map: %w", err)
	}
	return out, nil
}

// ReadEvent blocks until the next event arrives. It returns ringbuf.ErrClosed
// once the session is closed, which is how a reading goroutine is stopped.
func (s *Session) ReadEvent() (Event, error) {
	if s.reader == nil {
		return Event{}, errors.New("this session was not opened in ModeStream")
	}

	record, err := s.reader.Read()
	if err != nil {
		return Event{}, err
	}

	var raw syscountEvent
	// The generated type carries structs.HostLayout and the kernel wrote it
	// with the host's own alignment, so the native byte order is the correct
	// one to read it back with.
	if err := binary.Read(bytes.NewReader(record.RawSample), binary.NativeEndian, &raw); err != nil {
		return Event{}, fmt.Errorf("decode a %d byte sample: %w", len(record.RawSample), err)
	}

	return Event{
		// bpf_ktime_get_ns is unsigned nanoseconds since boot and Duration is
		// signed; they part company after 292 years of uptime.
		//nolint:gosec // see above
		SinceBoot: time.Duration(raw.TimestampNs),
		TGID:      raw.Tgid,
		PID:       raw.Pid,
		Syscall:   raw.SyscallNr,
		Comm:      commToString(raw.Comm),
	}, nil
}

// Drops reports what the kernel side discarded.
func (s *Session) Drops() (Drops, error) {
	var d Drops
	for _, slot := range []struct {
		index syscountStatSlot
		into  *uint64
	}{
		{syscountStatSlotSTAT_RINGBUF_FULL, &d.RingbufFull},
		{syscountStatSlotSTAT_COUNTS_FULL, &d.CountsFull},
	} {
		if err := s.objs.Stats.Lookup(uint32(slot.index), slot.into); err != nil {
			return Drops{}, fmt.Errorf("read drop counter %d: %w", slot.index, err)
		}
	}
	return d, nil
}

// StopReading unblocks ReadEvent and leaves everything else alone.
//
// Closing the whole session to stop a read is the obvious move and it takes
// the evidence down with it: the record of what the kernel dropped lives in a
// map, and a closed map answers "bad file descriptor" -- so the one number
// that says whether the results can be trusted is unavailable at exactly the
// moment the results are being reported. That is not a hypothetical; it is
// what this package did until a run printed it instead of the drop count.
//
// Safe from any goroutine and safe to call more than once.
func (s *Session) StopReading() error {
	var err error
	s.stopOnce.Do(func() {
		if s.reader != nil {
			err = s.reader.Close()
		}
	})
	return err
}

// Close detaches the program and releases every kernel object. It is safe to
// call more than once, from any goroutine, and on a partially constructed
// session -- which is what makes it usable both as the deferred cleanup in
// Open and as the way to interrupt a blocked ReadEvent.
//
// Nothing is set to nil on the way out. Clearing the reader would make the
// ReadEvent that is unblocked by this call report "not opened in ModeStream"
// instead of "closed", which sends the reader looking for a bug in how the
// session was opened rather than at the shutdown that just happened.
func (s *Session) Close() error {
	s.closeOnce.Do(func() {
		var errs []error
		// The reader first: closing it is what releases a blocked Read, and
		// doing it after the maps would tear down what that read is using.
		errs = append(errs, s.StopReading())
		if s.link != nil {
			errs = append(errs, s.link.Close())
		}
		errs = append(errs, s.objs.Close())
		s.closeErr = errors.Join(errs...)
	})
	return s.closeErr
}

// commToString trims the kernel's fixed 16-byte thread name at its first NUL.
func commToString(comm [16]int8) string {
	b := make([]byte, 0, len(comm))
	for _, c := range comm {
		if c == 0 {
			break
		}
		// The kernel wrote bytes into a char array; Go's cgo-free mirror of
		// that array is int8 on this platform, so this is a reinterpretation
		// rather than a numeric conversion.
		//nolint:gosec // see above
		b = append(b, byte(c))
	}
	return string(b)
}

// explainVerifier renders a load failure with the verifier's own log, which
// only appears under %+v and is the only part of the error worth reading.
func explainVerifier(err error) string {
	var ve *ebpf.VerifierError
	if errors.As(err, &ve) {
		return fmt.Sprintf("%+v", ve)
	}
	return err.Error()
}

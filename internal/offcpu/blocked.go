package offcpu

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/cilium/ebpf"

	"github.com/FranciscoPLoureiro/wallclock/internal/ksyms"
)

// maxStackDepth mirrors MAX_STACK_DEPTH in the BPF program. A stack shorter
// than that is padded with zeroes, which is how the end is found.
const maxStackDepth = 32

// Blocked is one thread's time spent waiting on one thing.
type Blocked struct {
	TID uint32
	// Reason is what the stack was classified as.
	Reason Reason
	// Stack is the kernel stack the thread stopped on, innermost frame
	// first, resolved to names.
	Stack []string
	// Duration is how long this thread spent stopped there, summed over
	// every time it happened.
	Duration time.Duration
}

// Folded renders the stack in the format flamegraph.pl reads: frames
// outermost first, separated by semicolons, followed by a count.
//
// Microseconds rather than nanoseconds as the count. A flame graph's width is
// the count, and nanoseconds make every rectangle a number nobody can read
// while changing nothing about the picture.
func (b Blocked) Folded(comm string) string {
	var sb strings.Builder
	sb.WriteString(comm)
	for i := len(b.Stack) - 1; i >= 0; i-- {
		sb.WriteByte(';')
		sb.WriteString(b.Stack[i])
	}
	fmt.Fprintf(&sb, " %d", b.Duration.Microseconds())
	return sb.String()
}

// BlockedReasons returns every thread's blocked time, split by what it was
// blocked on.
//
// The totals here should add up, per thread, to that thread's Blocked --
// they are the same intervals counted a second way. Where they do not, some
// blocked time has no reason attached to it, which the report says rather
// than quietly rounding the categories up to the total.
// The threads it is given are used for two things: to attribute the wait each
// one is still in the middle of, and to drop the rows belonging to threads
// that are gone. Pass the slice from Threads, read at the same moment.
func (s *Session) BlockedReasons(symbols *ksyms.Table, threads []Thread) ([]Blocked, error) {
	if symbols == nil {
		return nil, errors.New("a symbol table is required to name stacks")
	}

	live := make(map[uint32]struct{}, len(threads))
	for _, t := range threads {
		live[t.TID] = struct{}{}
	}

	var (
		out   []Blocked
		stale []offcpuBlockedKey
		key   offcpuBlockedKey
		total uint64
	)
	iter := s.objs.BlockedBy.Iterate()
	for iter.Next(&key, &total) {
		if _, ok := live[key.Tid]; !ok {
			// The thread is gone and its entry has been drained, so nothing
			// will ever ask about these rows again. Left in place they are a
			// map that only grows, on a machine with process churn.
			stale = append(stale, key)
			continue
		}
		frames, err := s.stackFrames(key.StackId, symbols)
		if err != nil {
			return nil, err
		}
		out = append(out, Blocked{
			TID:      key.Tid,
			Reason:   Classify(frames),
			Stack:    frames,
			Duration: time.Duration(total), //nolint:gosec // ns, signed after 292 years
		})
	}
	if err := iter.Err(); err != nil {
		return nil, fmt.Errorf("iterate the blocked-by map: %w", err)
	}

	for _, dead := range stale {
		if err := s.objs.BlockedBy.Delete(dead); err != nil &&
			!errors.Is(err, ebpf.ErrKeyNotExist) {
			return nil, fmt.Errorf("remove the reasons of exited thread %d: %w",
				dead.Tid, err)
		}
	}

	// The wait each thread is still in the middle of. It has not ended, so
	// nothing has filed it, and it is usually the largest single thing a
	// thread did -- a report without it puts "unattributed" against every
	// thread that spent the window waiting.
	for _, t := range threads {
		if t.OpenBlocked <= 0 || t.OpenBlockedStack < 0 {
			continue
		}
		frames, err := s.stackFrames(t.OpenBlockedStack, symbols)
		if err != nil {
			return nil, err
		}
		out = append(out, Blocked{
			TID:      t.TID,
			Reason:   Classify(frames),
			Stack:    frames,
			Duration: t.OpenBlocked,
		})
	}
	return out, nil
}

// stackFrames reads one stack out of the kernel and names each address.
func (s *Session) stackFrames(id int32, symbols *ksyms.Table) ([]string, error) {
	if id < 0 {
		return nil, nil
	}

	var addrs [maxStackDepth]uint64
	if err := s.objs.Stacks.Lookup(uint32(id), &addrs); err != nil { //nolint:gosec // id is non-negative here
		// A stack can be evicted from the map between being recorded and
		// being read, which is a lost reason rather than a broken read: the
		// time is still counted, it just has nothing to say for itself.
		if errors.Is(err, ebpf.ErrKeyNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read stack %d: %w", id, err)
	}

	frames := make([]string, 0, len(addrs))
	for _, addr := range addrs {
		if addr == 0 {
			break // the kernel pads the tail with zeroes
		}
		if name := symbols.Resolve(addr); name != "" {
			frames = append(frames, name)
		}
	}
	return frames, nil
}

// ByReason totals blocked time per category for one thread, and reports how
// far those totals are from the thread's own blocked time.
//
// The difference is returned signed rather than hidden, and both directions
// mean something:
//
//   - positive is blocked time with no reason attached, because the stack
//     could not be captured or was evicted before it could be read. Folding
//     it into "other" would be presenting a gap as a finding.
//   - negative is the reasons adding to more than the wait they explain,
//     which cannot be true of the same intervals. **The cause of this is not
//     yet established.** It is measured at up to 7% of a thread's blocked
//     time on a busy machine, which is far too large to be the millisecond
//     the two reads are apart, so the read-skew explanation is ruled out
//     rather than assumed. The candidates still open are stack ids being
//     recycled between threads and an interval being credited twice on some
//     path through the state machine.
//
// It is reported either way rather than clamped, because a number that
// disagrees with itself in public is the only thing that gets the cause
// found. See NOTES.md.
func ByReason(blocked []Blocked, thread Thread) (map[Reason]time.Duration, time.Duration) {
	totals := make(map[Reason]time.Duration)
	var attributed time.Duration
	for _, b := range blocked {
		if b.TID != thread.TID {
			continue
		}
		totals[b.Reason] += b.Duration
		attributed += b.Duration
	}
	return totals, thread.Blocked - attributed
}

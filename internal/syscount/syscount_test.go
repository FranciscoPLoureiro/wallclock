package syscount_test

import (
	"errors"
	"os"
	"os/exec"
	"strconv"
	"testing"
	"time"

	"github.com/cilium/ebpf/ringbuf"
	"golang.org/x/sys/unix"

	"github.com/FranciscoPLoureiro/wallclock/internal/kerneltest"
	"github.com/FranciscoPLoureiro/wallclock/internal/syscount"
)

// unusedSyscall is a syscall number far above anything the kernel implements.
//
// This is what makes the counts in these tests exact rather than approximate.
// Filtering on a real syscall would count every other process on the machine
// making the same call, and asserting on that means a tolerance, a flaky
// test, and an assertion that proves the tool is roughly working. Nothing
// else on any machine calls 4444, so the expected count is a number rather
// than a range.
//
// It works because raw_syscalls:sys_enter fires in the syscall entry path
// before the number is checked against the table. The kernel then returns
// -ENOSYS and the caller is none the wiser, which is exactly the behaviour
// this needs: an observable event with no side effect.
const unusedSyscall = 4444

func makeUnusedSyscalls(t *testing.T, n int) {
	t.Helper()
	for range n {
		//nolint:staticcheck // the deprecation points at syscall.Syscall; this is x/sys/unix and deliberate
		if _, _, errno := unix.Syscall(unusedSyscall, 0, 0, 0); errno != unix.ENOSYS {
			t.Fatalf("syscall %d returned %v, expected ENOSYS -- the number is no longer unused",
				unusedSyscall, errno)
		}
	}
}

// The whole pipeline in one assertion: C compiled to BPF, loaded past the
// verifier, attached to a tracepoint, filtered inside the kernel, counted in
// a map, and read back as a number that is exactly right.
func TestCountsExactlyTheSyscallsMade(t *testing.T) {
	kerneltest.Root(t)

	const calls = 500

	session, err := syscount.Open(syscount.ModeCount, syscount.Filter{
		Syscall: syscount.OnlySyscall(unusedSyscall),
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer session.Close()

	makeUnusedSyscalls(t, calls)

	counts, err := session.Counts()
	if err != nil {
		t.Fatalf("counts: %v", err)
	}

	if len(counts) != 1 {
		t.Fatalf("counted %d processes, want only this one: %+v", len(counts), counts)
	}
	for pid, count := range counts {
		if count.Entries != calls {
			t.Errorf("pid %d made %d entries, want exactly %d", pid, count.Entries, calls)
		}
		// The kernel's name for us, not one read from /proc, which is the
		// difference that makes this work inside a pid namespace.
		if want := processName(t); count.Comm != want {
			t.Errorf("comm = %q, want %q", count.Comm, want)
		}
	}

	drops, err := session.Drops()
	if err != nil {
		t.Fatalf("drops: %v", err)
	}
	if drops.Any() {
		t.Errorf("the kernel dropped something during a 500 event test: %+v", drops)
	}
}

// The filter has to exclude, not merely include. A filter that let everything
// through would still pass the test above, because that test only makes the
// calls it counts.
func TestFilterByPIDExcludesEveryOtherProcess(t *testing.T) {
	kerneltest.Root(t)

	// Our own pid as the initial namespace numbers it, which is the only
	// numbering the in-kernel filter understands. Inside a container os.Getpid
	// returns something else entirely, so it is learned from the kernel: run
	// once filtered to a syscall only this process makes, and read back the
	// single key.
	pid := initialNamespacePID(t)

	session, err := syscount.Open(syscount.ModeCount, syscount.Filter{TGID: pid})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer session.Close()

	// Every other process on the machine is making syscalls throughout this,
	// and a child is started to guarantee it rather than hope for it.
	noise := exec.Command("/bin/sh", "-c", "for i in $(seq 200); do true; done; ls /proc >/dev/null")
	if err := noise.Run(); err != nil {
		t.Fatalf("running the noise process: %v", err)
	}
	makeUnusedSyscalls(t, 10)

	counts, err := session.Counts()
	if err != nil {
		t.Fatalf("counts: %v", err)
	}

	if len(counts) != 1 {
		t.Fatalf("the pid filter let %d processes through, want 1: %+v", len(counts), counts)
	}
	if _, ok := counts[pid]; !ok {
		t.Errorf("counts are keyed %v, want only pid %d", keysOf(counts), pid)
	}
	// No count assertion: this process is a Go program with a runtime making
	// syscalls of its own, so the number is real but not predictable. The
	// claim under test is which processes appear, not how many entries they
	// made -- that is the test above.
}

// The second transport, asserted the same way: N events in, N events out,
// with the fields the kernel filled in.
func TestStreamDeliversEveryEvent(t *testing.T) {
	kerneltest.Root(t)

	const calls = 200

	session, err := syscount.Open(syscount.ModeStream, syscount.Filter{
		Syscall: syscount.OnlySyscall(unusedSyscall),
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer session.Close()

	makeUnusedSyscalls(t, calls)

	// Stopping the reader from a timer is what unblocks the final Read: the
	// ring buffer has no end-of-stream, so a reader that has consumed
	// everything waits for an event that is not coming.
	time.AfterFunc(2*time.Second, func() { _ = session.StopReading() })

	var (
		received int
		last     syscount.Event
	)
	for {
		event, err := session.ReadEvent()
		if err != nil {
			if errors.Is(err, ringbuf.ErrClosed) {
				break
			}
			t.Fatalf("read event %d: %v", received+1, err)
		}
		received++
		last = event
		if received == calls {
			// Everything expected has arrived; stop rather than sit out the
			// timer, which would make this test two seconds slower for no
			// additional proof.
			if err := session.StopReading(); err != nil {
				t.Fatalf("stopping the reader: %v", err)
			}
		}
	}

	if received != calls {
		t.Errorf("received %d events, want %d", received, calls)
	}
	if last.Syscall != unusedSyscall {
		t.Errorf("event carries syscall %d, want %d", last.Syscall, unusedSyscall)
	}
	if last.Comm != processName(t) {
		t.Errorf("event carries comm %q, want %q", last.Comm, processName(t))
	}
	if last.SinceBoot <= 0 {
		t.Errorf("event carries timestamp %v, want the kernel's monotonic clock", last.SinceBoot)
	}
	// The tgid is the process and the pid is the thread. In a single-threaded
	// caller they are equal, and Go is never single-threaded -- so this is
	// only asserted to be populated, not to match anything.
	if last.TGID == 0 || last.PID == 0 {
		t.Errorf("event carries tgid %d and pid %d, want both populated", last.TGID, last.PID)
	}

	// Readable only because the reader was stopped rather than the session
	// closed. Asserting it here is what keeps that distinction from being
	// undone by a later tidy-up: closing the session first makes this line
	// fail with "bad file descriptor".
	drops, err := session.Drops()
	if err != nil {
		t.Fatalf("drops: %v", err)
	}
	if drops.RingbufFull != 0 {
		t.Errorf("the ring buffer dropped %d events while carrying %d", drops.RingbufFull, calls)
	}
}

func TestReadEventRefusesACountingSession(t *testing.T) {
	kerneltest.Root(t)

	session, err := syscount.Open(syscount.ModeCount, syscount.Filter{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer session.Close()

	if _, err := session.ReadEvent(); err == nil {
		t.Error("reading events from a counting session returned no error")
	}
}

// initialNamespacePID learns this process's pid as the kernel reports it,
// which inside a pid namespace is not os.Getpid().
func initialNamespacePID(t *testing.T) uint32 {
	t.Helper()

	session, err := syscount.Open(syscount.ModeCount, syscount.Filter{
		Syscall: syscount.OnlySyscall(unusedSyscall),
	})
	if err != nil {
		t.Fatalf("open a session to learn our own pid: %v", err)
	}
	defer session.Close()

	makeUnusedSyscalls(t, 1)

	counts, err := session.Counts()
	if err != nil {
		t.Fatalf("counts: %v", err)
	}
	if len(counts) != 1 {
		t.Fatalf("expected exactly one process to have called syscall %d, got %+v",
			unusedSyscall, counts)
	}
	for pid := range counts {
		if initial, err := syscount.InInitialPIDNamespace(); err == nil && initial {
			// Where the namespaces coincide the two numbers must agree, and
			// checking costs nothing: if they ever disagree here, the
			// assumption underneath this whole helper is wrong.
			if pid != uint32(os.Getpid()) {
				t.Fatalf("kernel reports pid %d, os.Getpid says %d, and we are in the "+
					"initial namespace where they must agree", pid, os.Getpid())
			}
		}
		return pid
	}
	panic("unreachable: the map had exactly one key")
}

func processName(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile("/proc/self/comm")
	if err != nil {
		t.Fatalf("read /proc/self/comm: %v", err)
	}
	name := string(raw)
	if n := len(name); n > 0 && name[n-1] == '\n' {
		name = name[:n-1]
	}
	return name
}

func keysOf(counts map[uint32]syscount.Count) []string {
	out := make([]string, 0, len(counts))
	for pid := range counts {
		out = append(out, strconv.FormatUint(uint64(pid), 10))
	}
	return out
}

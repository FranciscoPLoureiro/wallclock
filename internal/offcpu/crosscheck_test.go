package offcpu_test

import (
	"os"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/FranciscoPLoureiro/wallclock/internal/kerneltest"
	"github.com/FranciscoPLoureiro/wallclock/internal/offcpu"
)

// userHZ is the unit /proc reports CPU time in: hundredths of a second.
//
// Not CONFIG_HZ, which varies. USER_HZ is fixed at 100 in the kernel's
// userspace ABI precisely so that /proc does not change meaning underneath
// the programs reading it, which makes it safe to hardcode and unsafe to
// derive from anything else.
const userHZ = 100

// threadCPUTime reads the calling thread's own CPU time from /proc.
//
// This is the second opinion. Everything else in this package measures with
// the same tracepoints through the same state machine, so if that machine is
// systematically wrong, every test built on it agrees with the mistake. The
// kernel's own per-thread accounting is arrived at by a completely different
// route -- the scheduler updates it as it runs -- and has no way to be wrong
// in the same direction.
func threadCPUTime(t *testing.T) time.Duration {
	t.Helper()

	raw, err := os.ReadFile("/proc/thread-self/stat")
	if err != nil {
		t.Fatalf("read /proc/thread-self/stat: %v", err)
	}

	// The second field is the thread name in parentheses and may itself
	// contain spaces and parentheses, so the only safe place to start
	// splitting is after the last ')'. What follows is field 3 onwards.
	line := string(raw)
	close := strings.LastIndex(line, ")")
	if close < 0 || close+2 >= len(line) {
		t.Fatalf("cannot parse /proc/thread-self/stat: %q", line)
	}
	fields := strings.Fields(line[close+2:])

	// utime is field 14 and stime is field 15, so they are the twelfth and
	// thirteenth of what is left once the first two are gone.
	const utimeIndex, stimeIndex = 11, 12
	if len(fields) <= stimeIndex {
		t.Fatalf("/proc/thread-self/stat has %d fields after the name, want more than %d",
			len(fields), stimeIndex)
	}

	var ticks uint64
	for _, index := range []int{utimeIndex, stimeIndex} {
		value, err := strconv.ParseUint(fields[index], 10, 64)
		if err != nil {
			t.Fatalf("parse field %d of /proc/thread-self/stat (%q): %v",
				index, fields[index], err)
		}
		ticks += value
	}
	return time.Duration(ticks) * (time.Second / userHZ)
}

// The answer to "how do you know your tool is right".
//
// Every other test here compares the tool against arithmetic -- a thread that
// slept for 600 ms should show 600 ms blocked -- which catches a state machine
// that files time in the wrong column and misses one that miscounts time in
// every column equally. This one compares the tool against the kernel's own
// accounting of the same thread over the same interval, from a source that
// shares no code with it.
func TestOnCPUAgreesWithTheKernelsOwnAccounting(t *testing.T) {
	kerneltest.Root(t)

	session, err := offcpu.Open(0)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer session.Close()

	const (
		spin = 900 * time.Millisecond
		name = "wc-crosschk"
	)

	var kernelSays time.Duration
	done := make(chan struct{})
	go func() {
		defer close(done)
		// Named and pinned first, then straight into the loop, so that this
		// thread has almost no history before the interval being compared.
		// Both measurements then start from about zero and the absolute
		// values are comparable without having to align two "before" reads.
		nameThisThread(t, name)
		defer runtime.UnlockOSThread()

		deadline := time.Now().Add(spin)
		for time.Now().Before(deadline) { //nolint:revive // burning CPU is the point
		}
		kernelSays = threadCPUTime(t)
	}()
	<-done

	threads, err := session.Threads()
	if err != nil {
		t.Fatalf("threads: %v", err)
	}
	spinner, ok := findByComm(threads, name)
	if !ok {
		t.Fatalf("no thread named %q among %d observed", name, len(threads))
	}
	wallclockSays := spinner.OnCPU

	if kernelSays == 0 {
		t.Fatal("/proc reports no CPU time at all for a thread that just spun for " +
			spin.String())
	}

	// Declared tolerance, and each part of it is a real effect rather than
	// padding:
	//
	//   - /proc counts in 10 ms ticks, so it is quantised and can be a whole
	//     tick out at either end;
	//   - this tool starts counting at the thread's first scheduler event,
	//     which is a moment after the thread exists;
	//   - the goroutine names and pins itself before spinning, and that work
	//     is CPU time /proc counts and this tool may attribute to the moment
	//     before it first saw the thread.
	//
	// Twenty per cent covers all three with room to spare, and is still far
	// tighter than any error that would matter: a state machine crediting
	// on-CPU time to the wrong column, or double counting it, misses by
	// multiples rather than by a fifth.
	const tolerance = 0.20
	ratio := float64(wallclockSays) / float64(kernelSays)
	if ratio < 1-tolerance || ratio > 1+tolerance {
		t.Errorf("wallclock reports %v on-CPU, /proc reports %v: a ratio of %.2f, "+
			"outside the declared %.0f%%. Two independent measurements of the same "+
			"thread over the same interval disagree, and one of them is wrong",
			wallclockSays, kernelSays, ratio, 100*tolerance)
	}
	t.Logf("wallclock %v, kernel %v, ratio %.3f", wallclockSays, kernelSays, ratio)
}

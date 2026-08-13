package offcpu_test

import (
	"os"
	"testing"

	"github.com/FranciscoPLoureiro/wallclock/internal/kerneltest"
	"github.com/FranciscoPLoureiro/wallclock/internal/offcpu"
)

// Sessions have to be repeatable, and this is the test that says so.
//
// Each one attaches four programs, and every tracepoint attachment is a perf
// event whose ring buffer is charged against RLIMIT_MEMLOCK -- a limit that
// loading BPF stopped needing in 5.11 and that perf events never stopped
// needing. Under sudo, which applies the PAM limits, that budget ran out
// after three sessions and the fourth was refused with EPERM on its last
// attach. Run as root directly, where no PAM limits apply, it never happened.
//
// So this is not really about opening sessions in a loop. It is about a
// failure that only appears where the limits are tighter than on the machine
// the code was written on, which is every machine that matters.
func TestSessionsCanBeOpenedRepeatedly(t *testing.T) {
	kerneltest.Root(t)

	for i := range 8 {
		session, err := offcpu.Open(0)
		if err != nil {
			t.Fatalf("unfiltered session %d of 8: %v", i+1, err)
		}
		if err := session.Close(); err != nil {
			t.Fatalf("closing unfiltered session %d: %v", i+1, err)
		}
	}
}

// The filtered path attaches the same four programs and additionally seeds
// the target set, so it is exercised separately: a resource ceiling that only
// bites when a target is named would otherwise be found by whoever first
// pointed the tool at a pid.
func TestFilteredSessionsCanBeOpenedRepeatedly(t *testing.T) {
	kerneltest.Root(t)

	for i := range 4 {
		session, err := offcpu.Open(os.Getpid())
		if err != nil {
			t.Fatalf("filtered session %d of 4: %v", i+1, err)
		}
		if err := session.Close(); err != nil {
			t.Fatalf("closing filtered session %d: %v", i+1, err)
		}
	}
}

package offcpu_test

import (
	"os/exec"
	"testing"
	"time"

	"github.com/FranciscoPLoureiro/wallclock/internal/kerneltest"
	"github.com/FranciscoPLoureiro/wallclock/internal/offcpu"
)

// A process that lived for a millisecond must be reported as having lived for
// a millisecond.
//
// Before the exit handling, it was not. A thread that exits switches out in a
// non-runnable state, so its entry parked in BLOCKED and went on accruing for
// the rest of the session: `/bin/true`, alive for about a millisecond, was
// reported as blocked for 12.02 seconds of a 12 second window, sorted to the
// top of the report, and indistinguishable from a thread that really had been
// waiting all that time. On a machine with process churn there were 660 such
// entries where there were ninety threads.
//
// The measurement is the assertion: an exited thread's window closes when it
// died, not when the report was produced.
func TestAShortLivedProcessIsNotReportedAsBlockedForTheWindow(t *testing.T) {
	kerneltest.Root(t)

	session, err := offcpu.Open(0)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer session.Close()

	// Let the session settle, run something that exits immediately, then let
	// the window run on well past it. The gap afterwards is the point: it is
	// the time the dead thread used to accumulate.
	time.Sleep(100 * time.Millisecond)
	if err := exec.Command("/bin/true").Run(); err != nil {
		t.Fatalf("running /bin/true: %v", err)
	}
	const afterwards = 900 * time.Millisecond
	time.Sleep(afterwards)

	threads, err := session.Threads()
	if err != nil {
		t.Fatalf("threads: %v", err)
	}

	found := false
	for _, thread := range threads {
		if thread.Comm != "true" {
			continue
		}
		found = true

		if !thread.Exited {
			t.Errorf("thread %d (%s) is not marked as exited", thread.TID, thread.Comm)
		}
		// It lived for a moment. Anything approaching the time that passed
		// afterwards means its clock kept running after it died.
		if thread.Observed > afterwards/2 {
			t.Errorf("observed for %v, which is most of the %v that elapsed after it "+
				"exited: a dead thread is still accruing time",
				thread.Observed, afterwards)
		}
		if thread.Blocked > afterwards/2 {
			t.Errorf("blocked for %v of a life of %v", thread.Blocked, thread.Observed)
		}
		assertDecompositionCloses(t, thread)
	}
	if !found {
		t.Fatal("the /bin/true that just ran was not observed at all")
	}
}

// Exited entries are removed once they have been read, which is what bounds
// the map on a machine with process churn. Reading twice must not return the
// same corpse twice.
func TestExitedThreadsAreDrainedOnRead(t *testing.T) {
	kerneltest.Root(t)

	session, err := offcpu.Open(0)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer session.Close()

	for range 20 {
		if err := exec.Command("/bin/true").Run(); err != nil {
			t.Fatalf("running /bin/true: %v", err)
		}
	}
	time.Sleep(50 * time.Millisecond)

	first, err := session.Threads()
	if err != nil {
		t.Fatalf("first read: %v", err)
	}
	exitedFirst := countExited(first)
	if exitedFirst == 0 {
		t.Fatal("none of the twenty processes that just exited were seen to exit")
	}

	second, err := session.Threads()
	if err != nil {
		t.Fatalf("second read: %v", err)
	}
	// A handful may have exited between the two reads; what must not happen
	// is the first read's corpses coming back.
	if exitedSecond := countExited(second); exitedSecond >= exitedFirst {
		t.Errorf("first read saw %d exited threads and the second saw %d: "+
			"they are not being drained, so the map grows until it is full",
			exitedFirst, exitedSecond)
	}
}

func countExited(threads []offcpu.Thread) int {
	n := 0
	for _, thread := range threads {
		if thread.Exited {
			n++
		}
	}
	return n
}

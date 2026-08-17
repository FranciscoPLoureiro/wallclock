package offcpu_test

import (
	"testing"
	"time"

	"github.com/FranciscoPLoureiro/wallclock/internal/kerneltest"
	"github.com/FranciscoPLoureiro/wallclock/internal/ksyms"
	"github.com/FranciscoPLoureiro/wallclock/internal/offcpu"
)

// The reasons for a thread's waiting cannot add to more than the waiting they
// explain. They are the same intervals counted a second way.
//
// They did, by up to 18%, and the number was printed as a known defect for
// exactly as long as it took to find out why. A wait that is open when the
// thread totals are read can end a moment later; the kernel then files it as
// a completed row, and reading the rows *after* the totals counts that one
// interval twice. What lands in the gap between two reads is not the length
// of the gap -- it is the whole of whatever wait happened to close during it,
// which is why the magnitude was misleading and the first diagnosis wrong.
//
// The fix is an ordering: rows first, totals second, open waits added last.
// This test is what keeps it that way, because the two calls look
// interchangeable and are not.
func TestReasonsNeverExceedTheBlockedTimeTheyExplain(t *testing.T) {
	kerneltest.Root(t)

	symbols, err := ksyms.Load()
	if err != nil {
		t.Skipf("kernel symbols are unavailable: %v", err)
	}

	session, err := offcpu.Open(0)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer session.Close()

	// Twice, a couple of seconds apart. Once could pass on a quiet moment
	// where no wait happened to close in the gap; the defect this guards
	// against needs traffic through the state machine to show itself.
	for round := 1; round <= 2; round++ {
		time.Sleep(1500 * time.Millisecond)

		blocked, err := session.BlockedReasons(symbols)
		if err != nil {
			t.Fatalf("round %d, blocked reasons: %v", round, err)
		}
		threads, err := session.Threads()
		if err != nil {
			t.Fatalf("round %d, threads: %v", round, err)
		}
		blocked, err = session.AttributeOpenWaits(blocked, threads, symbols)
		if err != nil {
			t.Fatalf("round %d, attributing open waits: %v", round, err)
		}
		if len(threads) == 0 {
			t.Fatal("no threads were observed at all")
		}

		for _, thread := range threads {
			totals, unattributed := offcpu.ByReason(blocked, thread)
			if unattributed >= 0 {
				continue // time with no reason yet is the honest direction
			}
			t.Errorf("round %d: thread %d (%s) blocked %v, reasons total %v -- "+
				"%v more than the waiting they explain, which is the same "+
				"interval counted twice (%d categories)",
				round, thread.TID, thread.Comm, thread.Blocked,
				thread.Blocked-unattributed, -unattributed, len(totals))
		}
	}
}

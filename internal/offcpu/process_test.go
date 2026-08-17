package offcpu_test

import (
	"os"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/FranciscoPLoureiro/wallclock/internal/kerneltest"
	"github.com/FranciscoPLoureiro/wallclock/internal/offcpu"
	"github.com/FranciscoPLoureiro/wallclock/internal/pidns"
)

// Two threads of one process have to agree about which process that is.
//
// The assertion looks weak and is not, because of where the number comes
// from. Nothing in a scheduler event names the process of the thread the
// event is about: it is inferred from the task that happens to be running
// when the program fires, and that task is the right one only at
// sched_switch, and there only for the thread being switched *out*. Take it
// at a wakeup and it is whoever did the waking; take it for the thread being
// switched in and it is the one leaving. Both mistakes produce a plausible
// process id -- a real one, belonging to a real process -- for every thread,
// and neither shows up as an error anywhere.
//
// What they cannot survive is two threads of the same process being asked
// separately and having to give the same answer. Threads switching out on
// two different CPUs are preceded and followed by unrelated work, so a
// misattributed tgid is a different number each time.
func TestThreadsOfOneProcessAgreeOnTheirProcess(t *testing.T) {
	kerneltest.Root(t)

	session, err := offcpu.Open(0)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer session.Close()

	// Named, pinned, and made to block: a thread is only ever seen leaving a
	// CPU, so one that does nothing is never observed at all.
	names := []string{testThreadPrefix + "proc-a", testThreadPrefix + "proc-b"}
	var wg sync.WaitGroup
	for _, name := range names {
		wg.Add(1)
		go func() {
			defer wg.Done()
			nameThisThread(t, name)
			defer runtime.UnlockOSThread()
			for range 5 {
				time.Sleep(20 * time.Millisecond)
			}
		}()
	}
	wg.Wait()

	threads, err := session.Threads()
	if err != nil {
		t.Fatalf("threads: %v", err)
	}

	seen := make(map[string]offcpu.Thread, len(names))
	for _, name := range names {
		thread, ok := findByComm(threads, name)
		if !ok {
			t.Fatalf("no thread named %q among %d observed", name, len(threads))
		}
		if thread.TGID == 0 {
			t.Fatalf("thread %d (%s) reports no process at all, after blocking "+
				"five times: every one of those is a switch-out, which is the "+
				"event that is supposed to answer this", thread.TID, name)
		}
		seen[name] = thread
	}

	a, b := seen[names[0]], seen[names[1]]
	if a.TID == b.TID {
		t.Fatalf("both subjects are thread %d: the two goroutines were not "+
			"pinned to different threads and this test proves nothing", a.TID)
	}
	if a.TGID != b.TGID {
		t.Errorf("threads %d and %d are in the same process and report %d and %d: "+
			"the process is being read from whichever task happened to be "+
			"running rather than from the thread the event is about",
			a.TID, b.TID, a.TGID, b.TGID)
	}

	// The second opinion, where there is one to be had. In the initial
	// namespace the kernel's numbering and this process's own agree, so the
	// answer can be checked against a source that shares no code with any of
	// this. Everywhere else the two number the same threads differently and
	// comparing them would fail on a correct implementation.
	initial, err := pidns.InInitial()
	if err != nil {
		t.Fatalf("determine the pid namespace: %v", err)
	}
	if !initial {
		t.Logf("thread group %d, unchecked against /proc: not the initial pid "+
			"namespace, where getpid() and the kernel's numbering disagree by "+
			"design. CI runs this half.", a.TGID)
		return
	}
	if got, want := int(a.TGID), os.Getpid(); got != want {
		t.Errorf("the kernel files these threads under process %d, and this "+
			"process is %d", got, want)
	}
}

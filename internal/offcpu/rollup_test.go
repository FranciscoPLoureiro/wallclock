package offcpu_test

import (
	"testing"
	"time"

	"github.com/FranciscoPLoureiro/wallclock/internal/offcpu"
)

func TestRollupAddsThreadsIntoTheirProcess(t *testing.T) {
	const s = time.Second

	threads := []offcpu.Thread{
		{TID: 100, TGID: 100, Comm: "api", Observed: 10 * s, OnCPU: 2 * s, Blocked: 8 * s},
		{TID: 101, TGID: 100, Comm: "api", Observed: 10 * s, OnCPU: 1 * s, Blocked: 9 * s},
		{TID: 102, TGID: 100, Comm: "api", Observed: 4 * s, Runqueue: 1 * s, Blocked: 3 * s, Exited: true},
	}

	got := offcpu.Rollup(threads)
	if len(got) != 1 {
		t.Fatalf("got %d processes, want 1", len(got))
	}
	p := got[0]

	if p.TGID != 100 || p.Comm != "api" {
		t.Errorf("got process %d named %q, want 100 named api", p.TGID, p.Comm)
	}
	if p.Threads != 3 || p.Exited != 1 {
		t.Errorf("got %d threads of which %d exited, want 3 and 1", p.Threads, p.Exited)
	}
	// Thread-seconds, not wall clock: three threads watched for ten, ten and
	// four seconds have twenty-four thread-seconds between them.
	if p.Observed != 24*s {
		t.Errorf("observed %v, want %v", p.Observed, 24*s)
	}
	if p.OnCPU != 3*s || p.Blocked != 20*s || p.Runqueue != 1*s {
		t.Errorf("on-CPU %v, blocked %v, runqueue %v; want %v, %v, %v",
			p.OnCPU, p.Blocked, p.Runqueue, 3*s, 20*s, 1*s)
	}
	if p.Residual() != 0 {
		t.Errorf("the categories miss the observation by %v", p.Residual())
	}
}

// The group leader names the process, and where the leader was not observed
// the majority of its threads do.
func TestRollupNamesAProcessAfterItsLeader(t *testing.T) {
	withLeader := offcpu.Rollup([]offcpu.Thread{
		// A thread renamed for its own purposes, as a worker pool does.
		{TID: 201, TGID: 200, Comm: "worker-3"},
		{TID: 202, TGID: 200, Comm: "worker-4"},
		{TID: 200, TGID: 200, Comm: "postgres"},
	})
	if len(withLeader) != 1 || withLeader[0].Comm != "postgres" {
		t.Errorf("got %q, want the leader's name postgres", withLeader[0].Comm)
	}

	// The leader is a thread like any other and can go unobserved -- it need
	// only stay on its CPU, or off it, for the whole window.
	withoutLeader := offcpu.Rollup([]offcpu.Thread{
		{TID: 301, TGID: 300, Comm: "netdemo"},
		{TID: 302, TGID: 300, Comm: "netdemo"},
		{TID: 303, TGID: 300, Comm: "odd-one-out"},
	})
	if len(withoutLeader) != 1 || withoutLeader[0].Comm != "netdemo" {
		t.Errorf("got %q, want the majority name netdemo", withoutLeader[0].Comm)
	}
}

// Threads whose process is not known yet are collected, not discarded.
func TestRollupKeepsThreadsWithNoProcessYet(t *testing.T) {
	got := offcpu.Rollup([]offcpu.Thread{
		{TID: 400, TGID: 400, Comm: "known", Observed: time.Second},
		{TID: 401, TGID: 0, Comm: "not-yet", Observed: time.Second},
	})
	if len(got) != 2 {
		t.Fatalf("got %d processes, want 2: the thread with no process was dropped", len(got))
	}

	var total time.Duration
	for _, p := range got {
		total += p.Observed
	}
	if total != 2*time.Second {
		t.Errorf("the rollup accounts for %v of %v observed", total, 2*time.Second)
	}
}

// Busiest first, where busy is time spent anywhere other than running: the
// same order the thread report uses, for the same reason.
func TestRollupPutsTheMostDelayedProcessFirst(t *testing.T) {
	const s = time.Second

	got := offcpu.Rollup([]offcpu.Thread{
		{TID: 1, TGID: 1, Comm: "busy-cpu", Observed: 10 * s, OnCPU: 10 * s},
		{TID: 2, TGID: 2, Comm: "queued", Observed: 10 * s, Runqueue: 4 * s, OnCPU: 6 * s},
		{TID: 3, TGID: 3, Comm: "waiting", Observed: 10 * s, Blocked: 9 * s, OnCPU: 1 * s},
	})

	want := []string{"waiting", "queued", "busy-cpu"}
	for i, name := range want {
		if got[i].Comm != name {
			t.Errorf("position %d is %q, want %q", i, got[i].Comm, name)
		}
	}
}

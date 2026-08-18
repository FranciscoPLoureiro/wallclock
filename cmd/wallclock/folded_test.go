package main

import (
	"strings"
	"testing"
	"time"

	"github.com/FranciscoPLoureiro/wallclock/internal/offcpu"
)

// The folded output is keyed by TID and named from the thread list, so a
// stack belonging to a thread that is not in that list is still written --
// under a made-up name. That is the whole failure mode this guards: -cgroup
// and -comm filter the table, and if they do not also filter the stacks then
// a flame graph of one container is a flame graph of the machine.
func TestFoldedStacksAreLimitedToTheThreadsReported(t *testing.T) {
	blocked := []offcpu.Blocked{
		{TID: 10, Stack: []string{"futex_wait", "do_syscall"}, Duration: 3 * time.Second},
		{TID: 99, Stack: []string{"tcp_recvmsg", "do_syscall"}, Duration: time.Second},
	}
	// Only thread 10 survived the filter.
	threads := []offcpu.Thread{{TID: 10, Comm: "wanted"}}

	var out strings.Builder
	if err := writeFolded(&out, keepStacksOf(blocked, threads), threads); err != nil {
		t.Fatalf("writeFolded: %v", err)
	}

	got := out.String()
	if !strings.Contains(got, "wanted;do_syscall;futex_wait 3000000") {
		t.Errorf("the thread that was asked for is missing:\n%s", got)
	}
	if strings.Contains(got, "tcp_recvmsg") {
		t.Errorf("a stack from thread 99, which the filter excluded, was written:\n%s", got)
	}
	if strings.Contains(got, "tid-99") {
		t.Errorf("an excluded thread appears under an invented name:\n%s", got)
	}
}

// A thread that exited during the window has real stacks and no entry left
// in the table. Its time was measured and it must not be dropped -- the name
// is what is missing, not the work.
func TestStacksOfThreadsThatVanishedKeepTheirTime(t *testing.T) {
	blocked := []offcpu.Blocked{
		{TID: 7, Stack: []string{"io_schedule"}, Duration: time.Second},
	}
	threads := []offcpu.Thread{{TID: 7, Comm: ""}}

	var out strings.Builder
	if err := writeFolded(&out, keepStacksOf(blocked, threads), threads); err != nil {
		t.Fatalf("writeFolded: %v", err)
	}
	if got := out.String(); !strings.Contains(got, "tid-7;io_schedule 1000000") {
		t.Errorf("a nameless thread lost its stack:\n%s", got)
	}
}

func TestStacksWithNoFramesAreNotWritten(t *testing.T) {
	blocked := []offcpu.Blocked{
		{TID: 1, Stack: nil, Duration: time.Second},
		{TID: 1, Stack: []string{"futex_wait"}, Duration: time.Second},
	}
	threads := []offcpu.Thread{{TID: 1, Comm: "app"}}

	var out strings.Builder
	if err := writeFolded(&out, keepStacksOf(blocked, threads), threads); err != nil {
		t.Fatalf("writeFolded: %v", err)
	}
	if got := strings.Count(out.String(), "\n"); got != 1 {
		t.Errorf("wrote %d lines, want 1: a stack whose frames could not be read has\n"+
			"nothing to say and would draw a bar named after the thread", got)
	}
}

// Descending, so that the file is readable on its own before it ever reaches
// a renderer.
func TestFoldedStacksComeOutLongestFirst(t *testing.T) {
	blocked := []offcpu.Blocked{
		{TID: 1, Stack: []string{"short"}, Duration: time.Millisecond},
		{TID: 1, Stack: []string{"long"}, Duration: time.Hour},
	}
	threads := []offcpu.Thread{{TID: 1, Comm: "app"}}

	var out strings.Builder
	if err := writeFolded(&out, keepStacksOf(blocked, threads), threads); err != nil {
		t.Fatalf("writeFolded: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 2 || !strings.Contains(lines[0], "long") {
		t.Errorf("not sorted by duration:\n%s", out.String())
	}
}

// The subtitle is the only thing that travels with the picture, so what it
// says about the subject has to survive being the only context there is.
func TestTheSubtitleNamesWhatWasProfiled(t *testing.T) {
	tests := []struct {
		name                 string
		target, comm, cgroup string
		window               time.Duration
		want                 string
	}{
		{
			name:   "no filters",
			target: "every thread",
			window: 15 * time.Second,
			want:   "every thread, over 15s",
		},
		{
			name:   "a container id is abbreviated, not printed whole",
			target: "every thread",
			cgroup: "/sys/fs/cgroup/docker/" + strings.Repeat("a", 64),
			window: 25 * time.Second,
			want:   "every thread, in /sys/fs/cgroup/docker/aaaaaaaaaaaa…, over 25s",
		},
		{
			name:   "a short cgroup name is left alone",
			target: "every thread",
			cgroup: "/sys/fs/cgroup/system.slice",
			window: time.Minute,
			want:   "every thread, in /sys/fs/cgroup/system.slice, over 1m0s",
		},
		{
			name:   "every filter at once",
			target: "pid 42 and its threads",
			comm:   "api",
			cgroup: "/sys/fs/cgroup/docker/" + strings.Repeat("b", 64),
			window: 8 * time.Second,
			want:   `pid 42 and its threads, named like "api", in /sys/fs/cgroup/docker/bbbbbbbbbbbb…, over 8s`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := describeSubject(tc.target, tc.window, tc.comm, tc.cgroup)
			if got != tc.want {
				t.Errorf("describeSubject:\n got %q\nwant %q", got, tc.want)
			}
		})
	}
}

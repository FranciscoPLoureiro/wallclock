package main

import (
	"fmt"
	"io"
	"text/tabwriter"

	"github.com/FranciscoPLoureiro/wallclock/internal/offcpu"
)

// writeProcesses renders the decomposition one process at a time.
//
// The column is called thread-time and not observed, and the difference is
// the whole reason this view needs a warning under it. Nineteen threads
// watched for ten seconds have a hundred and ninety thread-seconds between
// them, and a thread pool that is mostly idle reports nearly all of them as
// blocked. That is not ten seconds of latency; it is nineteen threads waiting
// to be given something to do.
//
// The notes come after the table rather than under the rows they belong to.
// tabwriter computes a column width across a run of lines and starts again
// when the run is broken, so a note in the middle leaves every row after it
// aligned to a different grid -- and only some rows have notes, which makes
// the table look damaged rather than annotated.
func writeProcesses(out io.Writer, processes []offcpu.Process,
	pollers map[uint32]offcpu.Poller, top int,
) error {
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', tabwriter.AlignRight)
	fmt.Fprintln(w, "pid\tthreads\tthread-time\ton-cpu\trunqueue\tthrottled\tblocked\tunknown\t  command")

	var rotating []offcpu.Process
	for i, p := range processes {
		if i >= top {
			break
		}
		fmt.Fprintf(w, "%s\t%d\t%s\t%s\t%s\t%s\t%s\t%s\t  %s\n",
			processPID(p), p.Threads, round(p.Observed),
			percentOf(p.OnCPU, p.Observed),
			percentOf(p.Runqueue, p.Observed),
			percentOf(p.Throttled, p.Observed),
			percentOf(p.Blocked, p.Observed),
			percentOf(p.Unknown, p.Observed),
			processName(p))

		if poller, known := pollers[p.TGID]; known && poller.Rotating() {
			rotating = append(rotating, p)
		}
	}
	if err := w.Flush(); err != nil {
		return err
	}

	for _, p := range rotating {
		writeRotatingPollerNote(out, p, pollers[p.TGID])
	}
	return nil
}

// processPID renders the process id, or a dash for the threads nobody has
// been able to place yet.
//
// Those are not hidden. A thread sits there between its wakeup and the
// switch-out that first says which process it is in, and a reader comparing
// this total against the thread view needs to see where the difference went.
func processPID(p offcpu.Process) string {
	if p.TGID == 0 {
		return "-"
	}
	return fmt.Sprint(p.TGID)
}

func processName(p offcpu.Process) string {
	name := p.Comm
	switch {
	case p.TGID == 0:
		name = "(process not yet known)"
	case name == "":
		name = "(unnamed)"
	}
	if p.Exited > 0 {
		name = fmt.Sprintf("%s (%d of %d exited)", name, p.Exited, p.Threads)
	}
	return name
}

// writeRotatingPollerNote says what a rotating event loop means for the row
// it belongs to.
//
// Three claims, and each is there because a reader who stops at the table
// draws a wrong conclusion without it: that some thread here is the poller,
// that the blocked share is latency, and that a near-zero network figure
// means this process does not wait on the network.
func writeRotatingPollerNote(out io.Writer, p offcpu.Process, poller offcpu.Poller) {
	fmt.Fprintf(out, `
%s (%s) runs a rotating event loop: %d of its %d threads took a turn in it and
none is dedicated to it, so no thread here is "the netpoller". Most of the blocked
time above is an idle thread pool rather than the process waiting for anything, and
the %s on sockets is not how long it waited on the network -- its goroutines wait
in a place where no thread ever stops. See "the Go execution model" in the README.
`, processName(p), processPID(p), poller.Polling, poller.Threads, round(poller.Network))
}

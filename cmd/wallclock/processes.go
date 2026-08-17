package main

import (
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"github.com/FranciscoPLoureiro/wallclock/internal/offcpu"
)

// rotatingMark is what a process running a rotating event loop is tagged
// with in the table, and what the note underneath is keyed to.
const rotatingMark = " *"

// writeProcesses renders the decomposition one process at a time.
//
// The column is called thread-time and not observed, and the difference is
// the whole reason this view needs a warning under it. Nineteen threads
// watched for ten seconds have a hundred and ninety thread-seconds between
// them, and a thread pool that is mostly idle reports nearly all of them as
// blocked. That is not ten seconds of latency; it is nineteen threads waiting
// to be given something to do.
//
// The rotating event loops are marked in the table and explained once
// underneath, rather than annotated in place, for two reasons. tabwriter
// computes a column width over a run of lines and starts again when the run
// is broken, so a note between rows re-aligns everything after it to a
// different grid. And there are more of these than one expects: a first run
// against a machine with the ticket office on it printed the same four lines
// six times, for k6, dockerd, containerd and three containerd-shims, all of
// which are Go. An explanation repeated six times is not read once.
func writeProcesses(out io.Writer, processes []offcpu.Process,
	pollers map[uint32]offcpu.Poller, top int,
) error {
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', tabwriter.AlignRight)
	fmt.Fprintln(w, "pid\tthreads\tthread-time\ton-cpu\trunqueue\tthrottled\tblocked\tunknown\t  command")

	var rotating []string
	for i, p := range processes {
		if i >= top {
			break
		}
		name := processName(p)
		if poller, known := pollers[p.TGID]; known && poller.Rotating() {
			name += rotatingMark
			// The socket figure travels with the name. It is the number the
			// note is about -- the one that looks like good news and is not
			// -- and the table has no column for it, so without this the
			// sentence below refers to something the reader cannot see.
			rotating = append(rotating, fmt.Sprintf("%s (%s, %s on sockets)",
				p.Comm, processPID(p), round(poller.Network)))
		}
		fmt.Fprintf(w, "%s\t%d\t%s\t%s\t%s\t%s\t%s\t%s\t  %s\n",
			processPID(p), p.Threads, round(p.Observed),
			percentOf(p.OnCPU, p.Observed),
			percentOf(p.Runqueue, p.Observed),
			percentOf(p.Throttled, p.Observed),
			percentOf(p.Blocked, p.Observed),
			percentOf(p.Unknown, p.Observed),
			name)
	}
	if err := w.Flush(); err != nil {
		return err
	}

	writeRotatingPollerNote(out, rotating)
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

// writeRotatingPollerNote explains the mark, once, for everything wearing it.
//
// Three claims, and each is there because a reader who stops at the table
// draws a wrong conclusion without it: that some thread in these is the
// poller, that their blocked share is latency, and that a near-zero network
// figure means they do not wait on the network.
// Two whole sentences rather than a pronoun assembled from parts. Threading
// "it"/"them" and "runs"/"run" through one template costs four variables and
// produces text that reads like it was assembled from four variables, and the
// one thing this note has to do is be read.
func writeRotatingPollerNote(out io.Writer, marked []string) {
	switch len(marked) {
	case 0:
		return
	case 1:
		// The name goes on its own line in both forms. It is the only part
		// whose length is not known here, and left inline it pushes the
		// first sentence past any width the rest is wrapped to.
		fmt.Fprintf(out, `
%s runs a rotating event loop:
its threads take turns in one and none of them is dedicated to it, so no thread in it
is "the netpoller". Most of the blocked time above is an idle thread pool rather than
the process waiting for anything, and what it shows on sockets is not how long it
waited on the network -- its goroutines wait in a place where no thread ever stops.
See "the Go execution model" in the README.
`, marked[0])
	default:
		fmt.Fprintf(out, `
%s run a rotating event loop:
their threads take turns in one and none of them is dedicated to it, so no thread in
them is "the netpoller". Most of their blocked time above is an idle thread pool
rather than the process waiting for anything, and what they show on sockets is not
how long they waited on the network -- their goroutines wait in a place where no
thread ever stops. See "the Go execution model" in the README.
`, strings.Join(marked, ", "))
	}
}

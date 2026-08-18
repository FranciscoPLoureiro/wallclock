package main

import (
	"fmt"
	"io"
	"sort"
	"time"

	"github.com/FranciscoPLoureiro/wallclock/internal/offcpu"
)

// keepStacksOf drops the stacks of threads that are not being reported.
//
// The filters -- -pid, -cgroup, -comm -- narrow the thread table, and the
// blocked stacks are a separate list keyed by TID that nothing narrows. Left
// alone, the folded file of one container is the folded file of the machine,
// with every foreign thread written out under an invented tid-N name because
// the table has nothing to call it. Nothing about the result looks wrong.
func keepStacksOf(blocked []offcpu.Blocked, threads []offcpu.Thread) []offcpu.Blocked {
	reported := make(map[uint32]struct{}, len(threads))
	for _, t := range threads {
		reported[t.TID] = struct{}{}
	}
	kept := make([]offcpu.Blocked, 0, len(blocked))
	for _, b := range blocked {
		if _, ok := reported[b.TID]; ok {
			kept = append(kept, b)
		}
	}
	return kept
}

// writeFolded prints one line per stack, in the format flamegraph.pl reads:
//
//	comm;outermost;...;innermost microseconds
//
// An off-CPU flame graph rather than the usual kind. The width of a frame in
// a CPU flame graph is how long that code ran; here it is how long the thread
// sat still inside it, which is the half a CPU profiler cannot draw at all.
func writeFolded(out io.Writer, blocked []offcpu.Blocked, threads []offcpu.Thread) error {
	names := make(map[uint32]string, len(threads))
	for _, t := range threads {
		names[t.TID] = t.Comm
	}

	// Sorted by duration so the file reads usefully on its own, before it
	// ever reaches a flame graph. flamegraph.pl does not care about order.
	sorted := make([]offcpu.Blocked, len(blocked))
	copy(sorted, blocked)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].Duration > sorted[j].Duration
	})

	for _, b := range sorted {
		if len(b.Stack) == 0 {
			continue
		}
		comm := names[b.TID]
		if comm == "" {
			comm = fmt.Sprintf("tid-%d", b.TID)
		}
		if _, err := fmt.Fprintln(out, b.Folded(comm)); err != nil {
			return err
		}
	}
	return nil
}

// writeReasons prints what each thread was waiting for, under the thread's
// own line in the report.
func writeReasons(out io.Writer, blocked []offcpu.Blocked, thread offcpu.Thread) {
	totals, unattributed := offcpu.ByReason(blocked, thread)
	if len(totals) == 0 && unattributed == 0 {
		return
	}

	// Descending, so the answer is the first line rather than somewhere in
	// an alphabetical list.
	type row struct {
		reason offcpu.Reason
		total  time.Duration
	}
	rows := make([]row, 0, len(totals))
	for reason, total := range totals {
		rows = append(rows, row{reason, total})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].total > rows[j].total })

	for _, r := range rows {
		fmt.Fprintf(out, "            %-8s %s\n", r.reason, percentOf(r.total, thread.Observed))
	}
	// Printed whenever it is not zero, and labelled for which way it went.
	// Blocked time with no reason attached is a gap in the answer; reasons
	// adding to more than the wait they explain is the two having been read a
	// moment apart. Folding either into "other" would present a gap as a
	// finding.
	switch {
	case unattributed > 0:
		fmt.Fprintf(out, "            %-8s %s\n", "(no stack)",
			percentOf(unattributed, thread.Observed))
	case unattributed < 0:
		fmt.Fprintf(out, "            %-8s %s OVER the blocked total -- known defect\n",
			"(excess)", percentOf(-unattributed, thread.Observed))
	}
}

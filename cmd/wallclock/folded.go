package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/FranciscoPLoureiro/wallclock/internal/flamegraph"
	"github.com/FranciscoPLoureiro/wallclock/internal/offcpu"
)

// keepStacksOf drops the stacks of threads that are not being reported.
//
// The filters -- -pid, -cgroup, -comm -- narrow the thread table, and the
// blocked stacks are a separate list keyed by TID that nothing narrows. Left
// alone, a flame graph of one container is a flame graph of the machine,
// with every foreign thread drawn under an invented tid-N name because the
// table has nothing to call it. The picture would look entirely plausible.
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

// writeStackOutputs writes whichever of the two stack outputs were asked for.
//
// Both come from the same folded bytes rather than from two walks of the
// data, so the file and the picture cannot disagree about what was measured.
func writeStackOutputs(stdout io.Writer, foldedPath, flamePath, subject string,
	blocked []offcpu.Blocked, threads []offcpu.Thread,
) error {
	var folded bytes.Buffer
	if err := writeFolded(&folded, blocked, threads); err != nil {
		return fmt.Errorf("folding off-CPU stacks: %w", err)
	}

	if foldedPath != "" {
		if err := writeFile(foldedPath, func(w io.Writer) error {
			_, err := w.Write(folded.Bytes())
			return err
		}); err != nil {
			return fmt.Errorf("writing the folded stack file: %w", err)
		}
		fmt.Fprintf(stdout, "\nfolded off-CPU stacks written to %s\n", foldedPath)
	}

	if flamePath != "" {
		if err := writeFile(flamePath, func(w io.Writer) error {
			return flamegraph.Render(w, bytes.NewReader(folded.Bytes()), flamegraph.Options{
				Title:    "wallclock: off-CPU",
				Subtitle: subject,
			})
		}); err != nil {
			return fmt.Errorf("rendering the flame graph: %w", err)
		}
		fmt.Fprintf(stdout, "\noff-CPU flame graph written to %s\n", flamePath)
	}
	return nil
}

// describeSubject names what was profiled, for the subtitle of the flame
// graph. The picture leaves the terminal it was taken in, and a flame graph
// that does not say what it was pointed at or for how long is an
// illustration rather than a measurement.
func describeSubject(target string, window time.Duration, comm, cgroup string) string {
	parts := []string{target}
	if comm != "" {
		parts = append(parts, fmt.Sprintf("named like %q", comm))
	}
	if cgroup != "" {
		parts = append(parts, "in "+shortenCgroup(cgroup))
	}
	return fmt.Sprintf("%s, over %s", strings.Join(parts, ", "), window)
}

// shortenCgroup abbreviates a container id in a cgroup path, the way every
// other tool that has to print one does.
//
// A container id is sixty-four hex characters and only the first twelve
// distinguish it from anything else on the machine. Printed in full it is
// most of the line above a picture that has a width budget, and it is an
// identifier rather than information -- by the time anybody reads the image,
// that container has been gone for months. Ellipsised rather than silently
// cut, so nobody mistakes the short form for the whole thing.
func shortenCgroup(path string) string {
	cut := strings.LastIndexByte(path, '/')
	name := path[cut+1:]
	if len(name) <= 16 {
		return path
	}
	return path[:cut+1] + name[:12] + "…"
}

// writeFile creates a file, hands it to write, and closes it -- checking the
// close rather than deferring it away. A write that fails while flushing
// fails there and nowhere else, and a report that is silently half a file is
// worse than one that did not appear.
func writeFile(path string, write func(io.Writer) error) error {
	file, err := os.Create(path)
	if err != nil {
		return err
	}
	if err := write(file); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
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

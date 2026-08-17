package main

import (
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/FranciscoPLoureiro/wallclock/internal/ksyms"
	"github.com/FranciscoPLoureiro/wallclock/internal/offcpu"
	"github.com/FranciscoPLoureiro/wallclock/internal/syscount"
)

const profileUsage = `wallclock profile - split wall clock into on-CPU, runqueue and blocked

usage:
  wallclock profile [flags]

flags:
  -pid N          only this process and the threads it starts (default: everything)
  -for DURATION   how long to observe (default 10s)
  -top N          how many rows to show, busiest first (default 15)
  -comm SUBSTRING only threads whose name contains this
  -cgroup PATH    only threads in this cgroup, e.g. a container directory
                  under /sys/fs/cgroup
  -reasons        break blocked time down by what was being waited for
  -processes      add the threads up per process instead of listing them
  -folded FILE    write off-CPU folded stacks for flamegraph.pl
`

func runProfile(args []string) error {
	fs := flag.NewFlagSet("profile", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, profileUsage) }
	var (
		pid       = fs.Int("pid", 0, "")
		window    = fs.Duration("for", 10*time.Second, "")
		top       = fs.Int("top", 15, "")
		comm      = fs.String("comm", "", "")
		cgroup    = fs.String("cgroup", "", "")
		reasons   = fs.Bool("reasons", false, "")
		processes = fs.Bool("processes", false, "")
		folded    = fs.String("folded", "", "")
	)
	if err := fs.Parse(args); err != nil {
		return err
	}

	// A cgroup is named by a path and matched by the id the kernel gives it.
	// Unlike a pid that id means the same thing inside a container and out,
	// which makes this the one filter that works from anywhere -- and the
	// answer to the namespace problem that has turned up at every stage of
	// this project.
	var cgroupID uint64
	if *cgroup != "" {
		id, err := syscount.CgroupIDOf(*cgroup)
		if err != nil {
			return err
		}
		cgroupID = id
	}

	session, err := offcpu.Open(*pid)
	if err != nil {
		return err
	}
	defer session.Close()

	target := "every thread"
	if *pid > 0 {
		target = fmt.Sprintf("pid %d and its threads", *pid)
	}
	fmt.Fprintf(os.Stdout, "watching %s for %s\n", target, *window)
	if *pid > 0 {
		warnAboutPIDNamespace()
	}
	fmt.Fprintln(os.Stdout)

	if err := sleepOrInterrupt(*window); err != nil {
		return err
	}

	// Reasons before the thread totals, and the order is load bearing: a wait
	// that is open when the totals are read can end a moment later, and the
	// kernel then files it as a completed row. Reading the rows second counts
	// that interval twice. See BlockedReasons.
	var (
		blocked []offcpu.Blocked
		symbols *ksyms.Table
	)
	// The process view needs the reasons whether or not they were asked for:
	// what it has to say about a process is decided by where that process's
	// threads waited, and a view that stayed silent about a rotating event
	// loop unless a second flag was passed would be silent exactly when the
	// number above it is most misleading.
	wantReasons := *reasons || *folded != "" || *processes
	if wantReasons {
		var err error
		if symbols, err = ksyms.Load(); err != nil {
			return fmt.Errorf("naming kernel stacks: %w", err)
		}
		if blocked, err = session.BlockedReasons(symbols); err != nil {
			return err
		}
	}

	threads, err := session.Threads()
	if err != nil {
		return err
	}
	if wantReasons {
		if blocked, err = session.AttributeOpenWaits(blocked, threads, symbols); err != nil {
			return err
		}
	}
	// The two empty cases are reported separately. "Nothing matched your
	// filter" and "the tool saw nothing at all" look identical in a report
	// and mean opposite things -- one is a typo, the other is a broken
	// session -- and on a tool whose characteristic failure is a plausible
	// empty page, that distinction is the whole message.
	observed := len(threads)
	if observed == 0 {
		fmt.Fprintln(os.Stdout, "no threads were observed at all")
		return reportProfileDrops(session)
	}
	if cgroupID != 0 {
		threads = filterByCgroup(threads, cgroupID)
		if len(threads) == 0 {
			fmt.Fprintf(os.Stdout,
				"%d threads were observed, none in %s (cgroup id %d). A thread is "+
					"placed in its cgroup the first time it is seen leaving a CPU, "+
					"so one that never ran during the window is not there yet.\n",
				observed, *cgroup, cgroupID)
			return reportProfileDrops(session)
		}
	}
	if *comm != "" {
		threads = filterByComm(threads, *comm)
		if len(threads) == 0 {
			fmt.Fprintf(os.Stdout, "%d threads were observed, none named like %q\n",
				observed, *comm)
			return reportProfileDrops(session)
		}
	}

	// The process view answers a different question and replaces the table
	// rather than sitting beside it. A thread is what the scheduler moves; a
	// process is what somebody deployed, and a runtime that spreads one
	// service over sixteen interchangeable threads is only describable at the
	// second.
	if *processes {
		if err := writeProcesses(os.Stdout, offcpu.Rollup(threads),
			offcpu.Pollers(blocked, threads), *top); err != nil {
			return err
		}
		fmt.Fprintf(os.Stdout, "\n%d threads observed\n", len(threads))
		reportResiduals(threads)
		return reportProfileDrops(session)
	}

	// Busiest first, where busy means "spent the most wall clock somewhere
	// other than running" -- the whole point being that the interesting
	// threads are the ones a CPU profiler would rank last.
	sort.Slice(threads, func(i, j int) bool {
		wi := threads[i].Runqueue + threads[i].Blocked
		wj := threads[j].Runqueue + threads[j].Blocked
		if wi != wj {
			return wi > wj
		}
		return threads[i].TID < threads[j].TID
	})

	// The threads for which "the event loop" is a thread rather than a role.
	// Marking them is the thread-level half of phase 3, and it is the half
	// that has an answer only sometimes: a Go runtime produces none of these,
	// and the absence is the finding rather than a gap. Empty when the
	// reasons were not read, which is the same as knowing of none.
	eventLoops := offcpu.DedicatedPollers(blocked)

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', tabwriter.AlignRight)
	fmt.Fprintln(w, "tid\tobserved\ton-cpu\trunqueue\tthrottled\tblocked\tunknown\t  command")
	exited := 0
	for i, t := range threads {
		if t.Exited {
			exited++
		}
		if i >= *top {
			continue
		}
		// A thread that has gone is marked rather than dropped. Its time was
		// real and its numbers are final, and the reader needs to know the
		// difference between a thread blocked right now and one that stopped
		// existing three seconds ago.
		name := t.Comm
		if t.Exited {
			name += " (exited)"
		}
		// A thread in epoll is waiting to be given work, not waiting for work
		// to finish, and its blocked share reads as a slow dependency unless
		// it is said so.
		if eventLoops[t.TID] {
			name += " [event loop]"
		}
		fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%s\t%s\t%s\t  %s\n",
			t.TID,
			round(t.Observed),
			percentOf(t.OnCPU, t.Observed),
			percentOf(t.Runqueue, t.Observed),
			percentOf(t.Throttled, t.Observed),
			percentOf(t.Blocked, t.Observed),
			percentOf(t.Unknown, t.Observed),
			name)
		if *reasons {
			// Flushed per thread so the breakdown lands under its own row:
			// tabwriter buffers until it can align a block, and the reason
			// lines are not part of that block.
			if err := w.Flush(); err != nil {
				return err
			}
			writeReasons(os.Stdout, blocked, t)
		}
	}
	if err := w.Flush(); err != nil {
		return err
	}

	if *folded != "" {
		file, err := os.Create(*folded)
		if err != nil {
			return fmt.Errorf("creating the folded stack file: %w", err)
		}
		defer file.Close()
		if err := writeFolded(file, blocked, threads); err != nil {
			return fmt.Errorf("writing folded stacks: %w", err)
		}
		fmt.Fprintf(os.Stdout, "\nfolded off-CPU stacks written to %s\n", *folded)
	}

	fmt.Fprintf(os.Stdout, "\n%d threads observed", len(threads))
	if len(threads) > *top {
		fmt.Fprintf(os.Stdout, " (%d shown)", *top)
	}
	if exited > 0 {
		fmt.Fprintf(os.Stdout, ", %d of which had exited by the end of the window", exited)
	}
	fmt.Fprintln(os.Stdout)

	// The books have to balance, and saying so is cheap. A decomposition
	// that quietly failed to add up would be indistinguishable from one that
	// did, which is the failure this whole project is arranged against.
	reportResiduals(threads)
	return reportProfileDrops(session)
}

func filterByCgroup(threads []offcpu.Thread, id uint64) []offcpu.Thread {
	var kept []offcpu.Thread
	for _, t := range threads {
		if t.CgroupID == id {
			kept = append(kept, t)
		}
	}
	return kept
}

func filterByComm(threads []offcpu.Thread, substring string) []offcpu.Thread {
	var kept []offcpu.Thread
	for _, t := range threads {
		if strings.Contains(t.Comm, substring) {
			kept = append(kept, t)
		}
	}
	return kept
}

// percentOf renders a duration as its share of the observation, with the
// duration beside it. The percentage is what the categories are compared on;
// the absolute value is what makes a 90% share of four milliseconds
// recognisable as noise.
func percentOf(part, whole time.Duration) string {
	if whole <= 0 {
		return "-"
	}
	return fmt.Sprintf("%.1f%% %s", 100*float64(part)/float64(whole), round(part))
}

func round(d time.Duration) string {
	switch {
	case d >= time.Second:
		return d.Round(10 * time.Millisecond).String()
	case d >= time.Millisecond:
		return d.Round(100 * time.Microsecond).String()
	default:
		return d.Round(time.Microsecond).String()
	}
}

func reportResiduals(threads []offcpu.Thread) {
	var worst time.Duration
	for _, t := range threads {
		residual := t.Residual()
		if residual < 0 {
			residual = -residual
		}
		if residual > worst {
			worst = residual
		}
	}
	switch {
	case worst == 0:
		fmt.Fprintln(os.Stdout, "every decomposition sums to 100% of the time observed")
	case worst <= offcpu.ReadSkewTolerance:
		// Not an error, and worth printing rather than hiding: the number is
		// the cost of reading a map the kernel is still writing to, and a
		// reader who cannot see it has to take the "100%" on trust.
		fmt.Fprintf(os.Stdout,
			"every decomposition sums to 100%% of the time observed, to within %s "+
				"(the cost of reading a live map)\n", round(worst))
	default:
		fmt.Fprintf(os.Stdout,
			"WARNING: a decomposition is off by %s, past the %s that reading a live "+
				"map can explain. A transition is crediting time to no category.\n",
			round(worst), round(offcpu.ReadSkewTolerance))
	}
}

func reportProfileDrops(session *offcpu.Session) error {
	drops, err := session.Drops()
	if err != nil {
		return err
	}
	if !drops.Any() {
		fmt.Fprintln(os.Stdout, "no threads lost")
		return nil
	}
	if drops.EventsDropped > 0 {
		fmt.Fprintf(os.Stdout,
			"LOST %d scheduler events: the threads map is at max_entries, so some "+
				"threads are missing from this report entirely rather than misfiled. "+
				"That is events, not threads -- one untracked thread contributes one "+
				"per context switch\n", drops.EventsDropped)
	}
	if drops.TargetsFull > 0 {
		fmt.Fprintf(os.Stdout,
			"LOST %d newly created threads of the target: the target set is full\n",
			drops.TargetsFull)
	}
	if drops.CgroupsFull > 0 {
		fmt.Fprintf(os.Stdout,
			"LOST the throttling of %d cgroups: that map is at max_entries, so their "+
				"threads' waits are filed as ordinary runqueue delay -- the two "+
				"categories this tool exists to separate, merged again\n", drops.CgroupsFull)
	}
	return nil
}

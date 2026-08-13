package main

import (
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/FranciscoPLoureiro/wallclock/internal/offcpu"
)

const profileUsage = `wallclock profile - split wall clock into on-CPU, runqueue and blocked

usage:
  wallclock profile [flags]

flags:
  -pid N          only this process and the threads it starts (default: everything)
  -for DURATION   how long to observe (default 10s)
  -top N          how many threads to show, busiest first (default 15)
  -comm SUBSTRING only threads whose name contains this
`

func runProfile(args []string) error {
	fs := flag.NewFlagSet("profile", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, profileUsage) }
	var (
		pid    = fs.Int("pid", 0, "")
		window = fs.Duration("for", 10*time.Second, "")
		top    = fs.Int("top", 15, "")
		comm   = fs.String("comm", "", "")
	)
	if err := fs.Parse(args); err != nil {
		return err
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

	threads, err := session.Threads()
	if err != nil {
		return err
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
	if *comm != "" {
		threads = filterByComm(threads, *comm)
		if len(threads) == 0 {
			fmt.Fprintf(os.Stdout, "%d threads were observed, none named like %q\n",
				observed, *comm)
			return reportProfileDrops(session)
		}
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

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', tabwriter.AlignRight)
	fmt.Fprintln(w, "tid\tobserved\ton-cpu\trunqueue\tblocked\tunknown\t  command")
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
		fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%s\t%s\t  %s\n",
			t.TID,
			round(t.Observed),
			percentOf(t.OnCPU, t.Observed),
			percentOf(t.Runqueue, t.Observed),
			percentOf(t.Blocked, t.Observed),
			percentOf(t.Unknown, t.Observed),
			name)
	}
	if err := w.Flush(); err != nil {
		return err
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
	return nil
}

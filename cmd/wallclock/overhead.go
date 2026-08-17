package main

import (
	"flag"
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	"github.com/FranciscoPLoureiro/wallclock/internal/overhead"
)

const overheadUsage = `wallclock overhead - measure what this tool costs

Runs a synthetic load of two threads passing a byte through a pipe, with and
without a profiling session attached, at several event rates. Needs root.

usage:
  wallclock overhead [flags]

flags:
  -for DURATION   how long each run lasts (default 1s)
  -repeats N      runs per rate, median kept (default 3)
`

// overheadSweep crosses four orders of magnitude of context switches per
// second, and is chosen to find where the tool hurts rather than to flatter
// it.
//
// The bottom five points turn one pair's rate down with work between trips.
// The top three turn it up by running pairs at once, because a single pair
// bottoms out at whatever a wakeup costs -- about 140 µs a round trip on this
// host, or fourteen thousand switches a second, which is not enough load to
// find a limit with.
var overheadSweep = []overhead.Load{
	{Pairs: 1, Work: 20 * time.Millisecond},
	{Pairs: 1, Work: 2 * time.Millisecond},
	{Pairs: 1, Work: 200 * time.Microsecond},
	{Pairs: 1, Work: 20 * time.Microsecond},
	{Pairs: 1, Work: 0},
	{Pairs: 4, Work: 0},
	{Pairs: 16, Work: 0},
	{Pairs: 64, Work: 0},
}

func runOverhead(args []string) error {
	fs := flag.NewFlagSet("overhead", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, overheadUsage) }
	var (
		per     = fs.Duration("for", time.Second, "")
		repeats = fs.Int("repeats", 3, "")
	)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *repeats < 1 {
		return fmt.Errorf("-repeats must be at least 1, got %d", *repeats)
	}

	fmt.Fprintf(os.Stderr, "sweeping %d rates, %d runs each of %v, twice over...\n",
		len(overheadSweep), *repeats, *per)
	points, err := overhead.Measure(overheadSweep, *per, *repeats, func(load overhead.Load) {
		fmt.Fprintf(os.Stderr, "  %s...\n", load)
	})
	if err != nil {
		return err
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', tabwriter.AlignRight)
	fmt.Fprintln(w, "pairs\twork/trip\tswitches/s\tbaseline trips/s\tprofiled trips/s\toverhead\tnoise\tlost\t")
	for _, p := range points {
		lost := "none"
		if p.Drops.Any() {
			lost = fmt.Sprintf("%d events, %d stacks", p.Drops.EventsDropped, p.Drops.StacksLost)
		}
		// An overhead under the noise floor is printed as such rather than
		// as a number, because a number would be read as one.
		cost := fmt.Sprintf("%+.1f%%", 100*p.Cost())
		if !p.Measured() {
			cost = "under noise"
		}
		fmt.Fprintf(w, "%d\t%v\t%.0f\t%.0f\t%.0f\t%s\t%.1f%%\t%s\t\n",
			p.Load.Pairs, p.Load.Work, p.SwitchesPerSecond(),
			p.Baseline.Rate(), p.Profiled.Rate(), cost, 100*p.Noise(), lost)
	}
	if err := w.Flush(); err != nil {
		return err
	}

	fmt.Fprint(os.Stdout, `
Each round trip is two context switches, and the rate comes from the unprofiled
run so the axis does not move with the thing being measured. The session watches
every thread on the machine, which is the expensive case: filtering to one pid
makes the programs return early for everything else.

The noise column is two identical unprofiled runs measured against each other,
so it is what this method reports when the true answer is zero. An overhead
smaller than it has not been measured, and is shown as "under noise" rather
than as a number somebody would quote.
`)

	return reportChurn(os.Stdout)
}

// churnSweep is how many short-lived processes to start between reads.
//
// The threads map holds 16384 entries and exited threads are only released
// when userspace reads it, so the limit this tool actually has is threads per
// read rather than events per second. The sweep straddles that number on
// purpose.
var churnSweep = []int{2000, 8000, 20000}

// reportChurn finds the turnover at which the tool starts losing threads.
func reportChurn(out *os.File) error {
	fmt.Fprint(out, `
Then the limit that is actually reachable. Switch rate does not fill anything:
the same few threads switch and the map holding them stays the same size. New
threads do, because a slot is released only when userspace reads the map and
drains the ones that exited -- so the number that matters is threads between
reads, not events per second. This starts short-lived processes and reads once
at the end, which is the worst case and the shape of a build server.

`)
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', tabwriter.AlignRight)
	fmt.Fprintln(w, "processes\tper second\tthreads held\tevents dropped\tstacks lost\t")
	for _, count := range churnSweep {
		fmt.Fprintf(os.Stderr, "  %d short-lived processes...\n", count)
		c, err := overhead.MeasureChurn(count, 8)
		if err != nil {
			return err
		}
		fmt.Fprintf(w, "%d\t%.0f\t%d\t%d\t%d\t\n",
			c.Spawned, c.PerSecond(), c.Observed,
			c.Drops.EventsDropped, c.Drops.StacksLost)
	}
	return w.Flush()
}

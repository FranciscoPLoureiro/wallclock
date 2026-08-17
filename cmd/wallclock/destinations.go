package main

import (
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/FranciscoPLoureiro/wallclock/internal/netlat"
	"github.com/FranciscoPLoureiro/wallclock/internal/syscount"
)

const destinationsUsage = `wallclock destinations - how long each destination takes to answer

Times the interval from a request going out on a connection to the first bytes
of its answer coming back. No thread has to block for this to be measured,
which is why it works against a Go service, PostgreSQL and Redis alike.

usage:
  wallclock destinations [flags]

flags:
  -for DURATION    how long to observe (default 10s)
  -top N           how many destinations to show, slowest first (default 15)
  -cgroup PATH     only traffic from threads in this cgroup, e.g. a container
                   directory under /sys/fs/cgroup
  -names FILE      name destinations from a file of "address[:port] name" lines
  -serve ADDR      instead of printing once, serve Prometheus metrics at
                   ADDR/metrics until interrupted
`

func runDestinations(args []string) error {
	fs := flag.NewFlagSet("destinations", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, destinationsUsage) }
	var (
		window    = fs.Duration("for", 10*time.Second, "")
		top       = fs.Int("top", 15, "")
		cgroup    = fs.String("cgroup", "", "")
		nameFile  = fs.String("names", "", "")
		serveAddr = fs.String("serve", "", "")
	)
	if err := fs.Parse(args); err != nil {
		return err
	}

	var cgroupID uint64
	if *cgroup != "" {
		id, err := syscount.CgroupIDOf(*cgroup)
		if err != nil {
			return err
		}
		cgroupID = id
	}

	var names *netlat.Names
	if *nameFile != "" {
		var err error
		if names, err = netlat.LoadNames(*nameFile); err != nil {
			return err
		}
	}

	session, err := netlat.Open(cgroupID)
	if err != nil {
		return err
	}
	defer session.Close()

	if *serveAddr != "" {
		return serveMetrics(session, names, *serveAddr)
	}

	target := "every connection"
	if cgroupID != 0 {
		target = fmt.Sprintf("connections from %s", *cgroup)
	}
	fmt.Fprintf(os.Stdout, "watching %s for %s\n\n", target, *window)
	if err := sleepOrInterrupt(*window); err != nil {
		return err
	}

	stats, err := session.Destinations()
	if err != nil {
		return err
	}
	writeDestinations(os.Stdout, stats, names, *top)
	return reportDestinationDrops(os.Stdout, session)
}

func writeDestinations(out *os.File, stats []netlat.Stats, names *netlat.Names, top int) {
	if len(stats) == 0 {
		fmt.Fprintln(out, "no completed exchanges were seen. Either nothing spoke TCP "+
			"in this window, or what did never got an answer back on the same "+
			"connection -- a one-way stream has no round trip to time.")
		return
	}

	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', tabwriter.AlignRight)
	fmt.Fprintln(w, "destination\texchanges\tmean\tp50\tp99\tmax\t  name")
	for i, s := range stats {
		if i >= top {
			break
		}
		fmt.Fprintf(w, "%s\t%d\t%s\t%s\t%s\t%s\t  %s\n",
			s.Destination, s.Count,
			round(s.Mean()), round(s.Percentile(0.50)),
			round(s.Percentile(0.99)), round(s.Max),
			names.Lookup(s.Destination))
	}
	if err := w.Flush(); err != nil {
		fmt.Fprintf(os.Stderr, "wallclock: %v\n", err)
	}

	fmt.Fprintf(out, "\n%d destinations", len(stats))
	if len(stats) > top {
		fmt.Fprintf(out, " (%d shown)", top)
	}
	// The percentiles come off a log2 histogram, so they are the upper edge
	// of the bucket the sample landed in rather than an interpolation. Saying
	// so is the difference between a number and a number somebody quotes.
	fmt.Fprintln(out, ". p50 and p99 are bucket ceilings, not interpolations")
}

func reportDestinationDrops(out *os.File, session *netlat.Session) error {
	drops, err := session.Drops()
	if err != nil {
		return err
	}
	if drops.Unpaired > 0 {
		// Not a loss, and it says something about the traffic rather than
		// about the tool -- so it is reported separately from the things
		// that are losses, and explained.
		fmt.Fprintf(out, "%d answers arrived with no outstanding request on their "+
			"connection. That is normal for the receiving end of anything: a server "+
			"reading a request has sent nothing yet. A large share of it means the "+
			"traffic is not request-and-answer shaped, and then these numbers are "+
			"not round trips\n", drops.Unpaired)
	}
	if !drops.Any() {
		fmt.Fprintln(out, "nothing lost")
		return nil
	}
	if drops.DestinationsFull > 0 {
		fmt.Fprintf(out, "LOST %d exchanges: the destinations map is at max_entries, "+
			"so those peers are missing from this report entirely\n", drops.DestinationsFull)
	}
	if drops.SocketsFull > 0 {
		fmt.Fprintf(out, "LOST %d requests: the in-flight map is at max_entries, so "+
			"their answers had nothing to pair with\n", drops.SocketsFull)
	}
	if drops.ThreadsFull > 0 {
		fmt.Fprintf(out, "LOST %d reads: the per-thread map is at max_entries\n",
			drops.ThreadsFull)
	}
	return nil
}

// serveMetrics publishes the histograms until the process is interrupted.
//
// Read on each scrape rather than sampled on a timer, because that is what
// Prometheus expects and because the maps are cumulative: whenever they are
// read, they hold everything since the session opened. A counter that resets
// between scrapes would break every rate() built on it.
func serveMetrics(session *netlat.Session, names *netlat.Names, addr string) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, _ *http.Request) {
		stats, err := session.Destinations()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		if err := netlat.WriteMetrics(w, stats, names); err != nil {
			fmt.Fprintf(os.Stderr, "wallclock: writing metrics: %v\n", err)
		}
	})

	server := &http.Server{
		Addr:    addr,
		Handler: mux,
		// A scrape reads BPF maps and nothing else, so it cannot legitimately
		// take long. Without these a stuck client holds a connection open for
		// ever, which on a tool that runs as root is not a detail.
		ReadHeaderTimeout: 5 * time.Second,
		WriteTimeout:      30 * time.Second,
	}

	failed := make(chan error, 1)
	go func() {
		fmt.Fprintf(os.Stdout, "serving Prometheus metrics at http://%s/metrics\n", addr)
		fmt.Fprintln(os.Stdout, "the counters are cumulative from now; interrupt to stop")
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			failed <- err
		}
	}()

	interrupted := make(chan os.Signal, 1)
	signal.Notify(interrupted, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(interrupted)

	select {
	case err := <-failed:
		return err
	case <-interrupted:
		fmt.Fprintln(os.Stdout, "\ninterrupted")
	}
	return server.Close()
}

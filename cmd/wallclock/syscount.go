package main

import (
	"errors"
	"flag"
	"fmt"
	"math"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/cilium/ebpf/ringbuf"

	"github.com/FranciscoPLoureiro/wallclock/internal/pidns"
	"github.com/FranciscoPLoureiro/wallclock/internal/syscount"
)

const syscountUsage = `wallclock syscount - count syscall entries per process

usage:
  wallclock syscount [flags]

flags:
  -pid N          only this process
  -cgroup PATH    only this cgroup, given as a path under /sys/fs/cgroup
  -syscall N      only this syscall number
  -for DURATION   how long to observe (default 5s)
  -top N          how many processes to show (default 10)
  -stream         print individual events through the ring buffer instead of totals
`

func runSyscount(args []string) error {
	fs := flag.NewFlagSet("syscount", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, syscountUsage) }
	var (
		pid       = fs.Uint64("pid", 0, "")
		cgroup    = fs.String("cgroup", "", "")
		syscallNr = fs.Int64("syscall", -1, "")
		window    = fs.Duration("for", 5*time.Second, "")
		top       = fs.Int("top", 10, "")
		stream    = fs.Bool("stream", false, "")
	)
	if err := fs.Parse(args); err != nil {
		return err
	}

	// The kernel's pid is 32 bits and the filter compares 32 bits. Truncating
	// silently would turn a typo into a filter that matches an unrelated
	// process and reports its syscalls as yours.
	if *pid > math.MaxUint32 {
		return fmt.Errorf("pid %d is larger than a pid can be", *pid)
	}
	filter := syscount.Filter{TGID: uint32(*pid)}
	if *syscallNr >= 0 {
		filter.Syscall = syscount.OnlySyscall(*syscallNr)
	}
	if *cgroup != "" {
		id, err := syscount.CgroupIDOf(*cgroup)
		if err != nil {
			return err
		}
		filter.CgroupID = id
	}

	mode := syscount.ModeCount
	if *stream {
		mode = syscount.ModeStream
	}

	session, err := syscount.Open(mode, filter)
	if err != nil {
		return err
	}
	defer session.Close()

	fmt.Fprintf(os.Stdout, "watching %s for %s, %s\n", describeFilter(filter), *window, describeMode(mode))
	warnAboutPIDNamespace()
	fmt.Fprintln(os.Stdout)

	if *stream {
		return streamEvents(session, *window)
	}
	return countEntries(session, *window, *top)
}

func describeFilter(f syscount.Filter) string {
	var parts []string
	if f.TGID != 0 {
		parts = append(parts, "pid "+strconv.FormatUint(uint64(f.TGID), 10))
	}
	if f.CgroupID != 0 {
		parts = append(parts, "cgroup "+strconv.FormatUint(f.CgroupID, 10))
	}
	if f.Syscall != nil {
		parts = append(parts, "syscall "+strconv.FormatInt(*f.Syscall, 10))
	}
	if len(parts) == 0 {
		return "every process"
	}
	return strings.Join(parts, " and ")
}

func describeMode(m syscount.Mode) string {
	if m == syscount.ModeStream {
		return "streaming every event"
	}
	return "counting in the kernel"
}

// countEntries waits out the window and prints the busiest processes.
func countEntries(session *syscount.Session, window time.Duration, top int) error {
	if err := sleepOrInterrupt(window); err != nil {
		return err
	}

	counts, err := session.Counts()
	if err != nil {
		return err
	}

	type row struct {
		tgid    uint32
		entries uint64
		comm    string
	}
	rows := make([]row, 0, len(counts))
	var total uint64
	for tgid, count := range counts {
		rows = append(rows, row{tgid, count.Entries, count.Comm})
		total += count.Entries
	}
	// Descending by count, then by pid so equal counts do not reorder between
	// runs and make two outputs look different when they are not.
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].entries != rows[j].entries {
			return rows[i].entries > rows[j].entries
		}
		return rows[i].tgid < rows[j].tgid
	})

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', tabwriter.AlignRight)
	fmt.Fprintln(w, "pid\tentries\t  command")
	for i, r := range rows {
		if i >= top {
			break
		}
		fmt.Fprintf(w, "%d\t%d\t  %s\n", r.tgid, r.entries, r.comm)
	}
	if err := w.Flush(); err != nil {
		return err
	}

	fmt.Fprintf(os.Stdout, "\n%d entries across %d processes", total, len(rows))
	if len(rows) > top {
		fmt.Fprintf(os.Stdout, " (%d shown)", top)
	}
	fmt.Fprintln(os.Stdout)
	return reportDrops(session)
}

// streamEvents prints events until the window closes or the reader is
// interrupted.
func streamEvents(session *syscount.Session, window time.Duration) error {
	// StopReading rather than Close: the drop counters are read once this
	// loop ends, and they live in a map that Close would already have
	// released -- which is how this reported "bad file descriptor" instead of
	// the number that says whether the output is complete.
	done := time.AfterFunc(window, func() {
		if err := session.StopReading(); err != nil {
			fmt.Fprintf(os.Stderr, "stopping the reader: %v\n", err)
		}
	})
	defer done.Stop()

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', tabwriter.AlignRight)
	fmt.Fprintln(w, "since boot\tpid\ttid\tsyscall\t  command")

	var seen uint64
	for {
		event, err := session.ReadEvent()
		if err != nil {
			if errors.Is(err, ringbuf.ErrClosed) {
				break
			}
			return err
		}
		seen++
		fmt.Fprintf(w, "%.6fs\t%d\t%d\t%d\t  %s\n",
			event.SinceBoot.Seconds(), event.TGID, event.PID, event.Syscall, event.Comm)
	}
	if err := w.Flush(); err != nil {
		return err
	}

	fmt.Fprintf(os.Stdout, "\n%d events\n", seen)
	return reportDrops(session)
}

// reportDrops prints what the kernel discarded. It prints on success too:
// "nothing was lost" is a result, and a tool that only mentions loss when it
// happens leaves the reader unsure whether it was ever checked.
func reportDrops(session *syscount.Session) error {
	drops, err := session.Drops()
	if err != nil {
		return err
	}
	if !drops.Any() {
		fmt.Fprintln(os.Stdout, "no events lost")
		return nil
	}
	if drops.RingbufFull > 0 {
		fmt.Fprintf(os.Stdout, "LOST %d events: the ring buffer filled before userspace drained it\n",
			drops.RingbufFull)
	}
	if drops.CountsFull > 0 {
		fmt.Fprintf(os.Stdout, "LOST every syscall of %d processes: the counts map is at max_entries\n",
			drops.CountsFull)
	}
	return nil
}

// sleepOrInterrupt waits out the window, or returns early on Ctrl-C so a long
// observation can be cut short without losing what it has already collected.
func sleepOrInterrupt(window time.Duration) error {
	interrupted := make(chan os.Signal, 1)
	signal.Notify(interrupted, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(interrupted)

	select {
	case <-time.After(window):
	case <-interrupted:
		fmt.Fprintln(os.Stdout, "interrupted, reporting what was collected so far")
	}
	return nil
}

// warnAboutPIDNamespace says so, once, when the pids about to be printed are
// not the pids the reader can look up.
//
// Staying quiet here is the expensive option. Inside a container or a WSL
// distribution the kernel reports initial-namespace pids while /proc shows
// namespaced ones, and the two ranges overlap -- so `-pid` taken from `ps`
// silently matches nothing and the output is an idle process that is not
// idle. A warning is cheap; a wrong number that looks right is not.
func warnAboutPIDNamespace() {
	initial, err := pidns.InInitial()
	if err != nil || initial {
		return
	}
	fmt.Fprintln(os.Stdout,
		"note: this process is in its own pid namespace, and the kernel reports pids\n"+
			"      from the initial one. They will not match /proc or ps here, and -pid\n"+
			"      expects an initial-namespace pid.")
}

// Command wallclock will decompose the wall-clock time of a process into
// on-CPU, runqueue delay, cgroup throttling, network per destination and lock
// contention. None of that exists yet.
//
// What it does today is the two things phase 0 has to answer before any of it
// is worth building: whether this host can run BPF programs at all, and
// whether an object compiled here is one a real kernel accepts.
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/FranciscoPLoureiro/wallclock/internal/bpfload"
	"github.com/FranciscoPLoureiro/wallclock/internal/preflight"
)

const usage = `wallclock - kernel-level latency attribution

usage:
  wallclock preflight          check that this host meets the requirements
  wallclock load <object.o>    load a compiled BPF object and report the verifier output
  wallclock syscount [flags]   count syscall entries per process (-h for flags)
  wallclock profile [flags]    split wall clock into on-CPU, runqueue and blocked

All of them exit non-zero on failure, so any can gate a build.
`

func main() {
	flag.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	flag.Parse()

	if flag.NArg() == 0 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}

	var err error
	switch cmd := flag.Arg(0); cmd {
	case "preflight":
		err = runPreflight(os.Stdout)
	case "load":
		if flag.NArg() != 2 {
			fmt.Fprint(os.Stderr, usage)
			os.Exit(2)
		}
		err = runLoad(os.Stdout, flag.Arg(1))
	case "profile":
		err = runProfile(flag.Args()[1:])
	case "syscount":
		err = runSyscount(flag.Args()[1:])
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", cmd)
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}

	if err != nil {
		fmt.Fprintf(os.Stderr, "wallclock: %v\n", err)
		os.Exit(1)
	}
}

func runPreflight(out *os.File) error {
	report := preflight.Run()

	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "requirement\tstatus\tfound")
	for _, c := range report.Checks {
		status := "ok"
		if !c.OK {
			status = "FAIL"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\n", c.Name, status, c.Detail)
	}
	if err := w.Flush(); err != nil {
		return err
	}

	// The consequences are printed only for what failed. Printing all of them
	// every time would bury the one line that matters on the host where it
	// matters.
	failed := report.Failed()
	if len(failed) == 0 {
		fmt.Fprintln(out, "\nall requirements met")
		return nil
	}
	fmt.Fprintln(out)
	for _, c := range failed {
		fmt.Fprintf(out, "%s is not satisfied.\n  %s\n", c.Name, wrap(c.Consequence, 72, "  "))
	}
	return fmt.Errorf("%d of %d requirements not met", len(failed), len(report.Checks))
}

func runLoad(out *os.File, path string) error {
	loaded, err := bpfload.Object(path)
	if err != nil {
		// A verifier rejection is the most useful failure this command has,
		// and printing it as a bare error throws the log away.
		return errors.New(bpfload.Explain(err))
	}
	defer loaded.Close()

	for _, p := range loaded.Programs {
		fmt.Fprintf(out, "loaded %s (%v, %d instructions)\n", p.Name, p.Type, p.Instructions)
		// The verifier answers in several lines and the interesting one is
		// rarely the first, so every line is indented under the program it
		// belongs to rather than only the first.
		for _, line := range strings.Split(strings.TrimSpace(p.VerifierLog), "\n") {
			if line = strings.TrimSpace(line); line != "" {
				fmt.Fprintf(out, "  %s\n", line)
			}
		}
	}
	return nil
}

// wrap breaks a consequence across lines at word boundaries. The consequences
// are written as prose because they are read by someone whose build just
// failed, and prose that runs off the edge of a terminal is read by nobody.
func wrap(s string, width int, indent string) string {
	var b strings.Builder
	line := 0
	for i, word := range strings.Fields(s) {
		switch {
		case i == 0:
			b.WriteString(word)
			line = len(word)
		case line+1+len(word) > width:
			b.WriteString("\n" + indent + word)
			line = len(word)
		default:
			b.WriteString(" " + word)
			line += 1 + len(word)
		}
	}
	return b.String()
}

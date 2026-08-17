package main

import (
	"flag"
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/FranciscoPLoureiro/wallclock/internal/validate"
)

const validateUsage = `wallclock validate - measure this tool against known answers

Runs each scenario in turn, with subjects whose behaviour is decided in
advance, and prints what the tool reported beside what it should have.
Needs root, and takes about half a minute.

usage:
  wallclock validate [flags]

flags:
  -quiet   print the table only, without saying what is running
`

// runValidate is the table in the README, generated rather than transcribed.
//
// A validation table typed out by hand is a claim about a run nobody else can
// repeat. This one is a command, it exits non-zero when a scenario lands
// outside its declared tolerance, and so it can gate a build.
func runValidate(args []string) error {
	fs := flag.NewFlagSet("validate", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, validateUsage) }
	quiet := fs.Bool("quiet", false, "")
	if err := fs.Parse(args); err != nil {
		return err
	}

	scenarios := validate.Scenarios()
	// Progress on stderr, so that redirecting stdout gives a clean table and
	// still shows a reader that something is happening. Several of these
	// saturate the machine for three seconds and a silent minute reads like
	// a hang.
	onStart := func(s validate.Scenario) {
		fmt.Fprintf(os.Stderr, "running %s (%s)...\n", s.Name, s.GroundTruth)
	}
	if *quiet {
		onStart = nil
	}
	results := validate.RunAll(scenarios, onStart)

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "scenario\tcategory\texpected\treported\terror\ttolerance\t")
	for _, r := range results {
		if r.Skipped != "" {
			fmt.Fprintf(w, "%s\t%s\t%s\t-\tn/a\t%s\t\n",
				r.Name, r.Category, validate.Round(r.Expected), validate.Tolerance(r.Scenario))
			continue
		}
		if r.Err != nil {
			fmt.Fprintf(w, "%s\t%s\t%s\t-\terror\t%s\t\n",
				r.Name, r.Category, validate.Round(r.Expected), validate.Tolerance(r.Scenario))
			continue
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%+.1f%%\t%s\t%s\n",
			r.Name, r.Category,
			validate.Round(r.Expected), validate.Round(r.Reported),
			r.ErrorPercent(), validate.Tolerance(r.Scenario), r.Status())
	}
	if err := w.Flush(); err != nil {
		return err
	}

	// Ground truth under the table rather than in it. It is the most
	// important thing here -- a number without its provenance is an
	// assertion -- and it is also the longest column, so putting it in the
	// table costs the table.
	fmt.Fprintln(os.Stdout, "\nhow each expected value is known:")
	for _, r := range results {
		fmt.Fprintf(os.Stdout, "  %-18s %s\n", r.Name, r.GroundTruth)
	}

	var skipped, failed int
	for _, r := range results {
		switch {
		case r.Skipped != "":
			skipped++
			fmt.Fprintf(os.Stdout, "\n%s did not run here: %s\n", r.Name, r.Skipped)
		case r.Err != nil:
			failed++
			fmt.Fprintf(os.Stdout, "\n%s could not be carried out: %v\n", r.Name, r.Err)
		}
	}

	if n := validate.Failures(results); n > 0 {
		return fmt.Errorf("%d of %d scenarios landed outside the declared tolerance",
			n, len(results)-skipped)
	}
	fmt.Fprintf(os.Stdout, "\nevery scenario that ran here landed inside its declared tolerance")
	if skipped > 0 {
		fmt.Fprintf(os.Stdout, " (%d did not run)", skipped)
	}
	fmt.Fprintln(os.Stdout)
	return nil
}

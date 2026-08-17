// Package validate measures this tool against numbers that were known before
// it ran.
//
// Every other test in this repository compares wallclock against arithmetic
// derived from what wallclock itself observed, or against a second reading of
// the same kernel counters. Both are worth having and neither answers the
// question a reader actually has, which is whether the numbers mean anything
// at all. Here the answer is decided first -- a thread will sleep for exactly
// two seconds, a cgroup will be allowed exactly half a CPU, a socket will be
// delayed by exactly 50 ms in each direction -- and the tool is then asked
// what it saw.
//
// Deliberately free of any dependency on testing, so that the same scenarios
// produce the table in the README, run under `wallclock validate` on a
// machine nobody has a checkout on, and are asserted in CI, without three
// implementations drifting apart. A tolerance that lives in one place cannot
// be quietly loosened in another.
package validate

import (
	"fmt"
	"time"
)

// Scenario is one measurement whose answer is known in advance.
type Scenario struct {
	// Name is what the table calls it.
	Name string
	// Category is the column of the decomposition under test. A scenario
	// that validates blocked time says nothing about throttling, and a table
	// that did not say which was which would read as though each row
	// validated the whole tool.
	Category string
	// GroundTruth says *how* the expected value is known, in a few words. It
	// is the most important field here: a number without its provenance is
	// an assertion, and the whole point of this package is that these are
	// not assertions.
	GroundTruth string
	// Expected is what the mechanism above says the tool should report.
	Expected time.Duration
	// Low and High are the declared tolerance, inclusive. "Approximately
	// 50 ms" is not an assertion; "between 48 and 55 ms" is. They are set
	// per scenario rather than as one global percentage because the error in
	// each has a different shape and a different sign, and the comment on
	// each scenario says which.
	Low, High time.Duration
	// Run performs the scenario and reports what the tool measured.
	Run func() (time.Duration, error)
	// Needs, when non-empty, is what the host lacks -- checked before the
	// run so a missing kernel module reads as "not applicable here" rather
	// than as a failure of the tool.
	Needs func() error
}

// Result is one scenario, run.
type Result struct {
	Scenario
	// Reported is what wallclock said.
	Reported time.Duration
	// Skipped is why the host could not run this, if it could not.
	Skipped string
	// Err is a failure to carry the scenario out at all, which is not the
	// same as the tool getting the answer wrong and is not reported as
	// though it were.
	Err error
}

// Pass reports whether the tool landed inside the declared tolerance.
func (r Result) Pass() bool {
	if r.Err != nil || r.Skipped != "" {
		return false
	}
	return r.Reported >= r.Low && r.Reported <= r.High
}

// ErrorPercent is how far the reported value is from the expected one, signed,
// so that a reader can see whether the tool over- or under-reports. The two
// have different causes and only one of them is alarming.
func (r Result) ErrorPercent() float64 {
	if r.Expected == 0 {
		return 0
	}
	return 100 * float64(r.Reported-r.Expected) / float64(r.Expected)
}

// Status is one word for the table.
func (r Result) Status() string {
	switch {
	case r.Skipped != "":
		return "n/a"
	case r.Err != nil:
		return "error"
	case r.Pass():
		return "ok"
	default:
		return "OUT"
	}
}

// Run carries out one scenario, checking first that the host can.
func Run(s Scenario) Result {
	r := Result{Scenario: s}
	if s.Needs != nil {
		if err := s.Needs(); err != nil {
			r.Skipped = err.Error()
			return r
		}
	}
	reported, err := s.Run()
	r.Reported, r.Err = reported, err
	return r
}

// RunAll carries out every scenario in order.
//
// In order, and one at a time, because several of them saturate the machine
// on purpose: a runqueue scenario that ran beside a throttling scenario would
// measure the two of them together and both would be wrong.
func RunAll(scenarios []Scenario, onStart func(Scenario)) []Result {
	results := make([]Result, 0, len(scenarios))
	for _, s := range scenarios {
		if onStart != nil {
			onStart(s)
		}
		results = append(results, Run(s))
	}
	return results
}

// Failures counts the results that ran and landed outside their tolerance.
// Scenarios the host could not run are not failures of the tool.
func Failures(results []Result) int {
	n := 0
	for _, r := range results {
		if r.Skipped == "" && !r.Pass() {
			n++
		}
	}
	return n
}

// Round renders a duration at a precision that does not imply more than was
// measured. Nanoseconds on a two-second interval is false confidence.
func Round(d time.Duration) string {
	switch {
	case d >= time.Second:
		return d.Round(time.Millisecond).String()
	case d >= time.Millisecond:
		return d.Round(100 * time.Microsecond).String()
	default:
		return d.Round(time.Microsecond).String()
	}
}

// Tolerance renders the declared band.
func Tolerance(s Scenario) string {
	return fmt.Sprintf("%s to %s", Round(s.Low), Round(s.High))
}

package validate

import (
	"fmt"
	"runtime"
	"time"

	"github.com/FranciscoPLoureiro/wallclock/internal/offcpu"
)

// window is how long each scenario runs its subjects for.
//
// The same for all of them, so the table's rows are comparable, and long
// enough that the fixed costs -- starting processes, the moment between a
// thread existing and first being seen -- are a small share of it. Three
// seconds of every scenario is about twenty seconds of wall clock for the
// whole table, which is short enough that it gets run.
const window = 3 * time.Second

// Scenarios returns every measurement with a known answer, in the order they
// should be run.
func Scenarios() []Scenario {
	return []Scenario{
		sleepScenario(),
		runqueueScenario(),
		throttlingScenario(),
		futexScenario(),
		netemScenario(),
	}
}

// sleepScenario: a thread that sleeps for a known total.
//
// The simplest thing a profiler can get wrong, and the one every off-CPU
// profiler already does, so it is the floor rather than the interesting case.
// It is here because a table whose easiest row is missing invites the question
// of whether it was left out for a reason.
func sleepScenario() Scenario {
	const naps = 30
	const nap = window / naps

	return Scenario{
		Name:        "sleep",
		Category:    "blocked",
		GroundTruth: fmt.Sprintf("%d sleeps of %v", naps, nap),
		Expected:    window,
		// The error here has a sign. A sleep may return late and never
		// early, so the true blocked time is at or above the nominal; the
		// tool can only miss time at the start, never invent it. So the
		// band is tight below and loose above, and neither bound is
		// symmetrical padding.
		Low:  window - 30*time.Millisecond,
		High: window + 250*time.Millisecond,
		Run: func() (time.Duration, error) {
			obs, err := observe("sleeper", false, nil, func() error {
				done := make(chan error, 1)
				go func() {
					unpin, err := nameThisThread("sleeper")
					if err != nil {
						done <- err
						return
					}
					defer unpin()
					for range naps {
						time.Sleep(nap)
					}
					done <- nil
				}()
				return <-done
			})
			if err != nil {
				return 0, err
			}
			return obs.sum(func(t offcpu.Thread) time.Duration { return t.Blocked }), nil
		},
	}
}

// runqueueScenario: more threads that never sleep than the machine has CPUs.
//
// Ground truth is division. C cores shared by T threads that always want one
// give each thread C/T of a CPU, so each spends 1 - C/T of its life ready and
// waiting -- and *that* is the quantity no off-CPU profiler separates from
// waiting on a socket.
func runqueueScenario() Scenario {
	cores := runtime.NumCPU()
	threads := 3 * cores
	share := 1 - float64(cores)/float64(threads)

	return Scenario{
		Name:        "runqueue delay",
		Category:    "runqueue",
		GroundTruth: fmt.Sprintf("%d spinners on %d cores, so 1-%d/%d of each life is queued", threads, cores, cores, threads),
		Expected:    time.Duration(share * float64(window)),
		// Wider than the others, and the width is the rest of the machine.
		// This scenario's arithmetic assumes the subjects are the only
		// runnable work, which is never quite true: the profiler, the
		// shell that started it and whatever else the host is doing all
		// take a slice, which pushes the measured share *up*, not down.
		// Phase 2 measured 31.6% where the arithmetic said 33.3%.
		Low:  time.Duration(0.90 * share * float64(window)),
		High: time.Duration(1.08 * share * float64(window)),
		Run: func() (time.Duration, error) {
			var group *spinners
			obs, err := observe("spin", false, nil, func() error {
				var err error
				if group, err = startSpinners(threads, "spin"); err != nil {
					return err
				}
				time.Sleep(window)
				group.stop()
				return nil
			})
			if group != nil {
				group.stop()
			}
			if err != nil {
				return 0, err
			}

			// Scaled by what was actually observed rather than taken raw.
			// Forty-eight processes do not start at the same instant, so
			// the late ones are watched for less than the window and
			// contribute proportionally less of everything. The claim under
			// test is the *share* of a runnable thread's life spent queued;
			// this reports that share against the nominal window so the
			// table can carry a duration.
			queued := obs.sum(func(t offcpu.Thread) time.Duration { return t.Runqueue })
			observed := obs.sum(func(t offcpu.Thread) time.Duration { return t.Observed })
			if observed == 0 {
				return 0, fmt.Errorf("%d spinners were observed for no time at all", len(obs.threads))
			}
			return time.Duration(float64(window) * float64(queued) / float64(observed)), nil
		},
	}
}

// throttlingScenario: a cgroup allowed exactly half a CPU.
//
// The new category, and the easiest to be sure about: the quota is a number
// written to a file, so a thread that never stops asking for CPU can have
// exactly that fraction of one and must spend the rest ready and forbidden.
// Checked twice -- against the arithmetic here, and against the kernel's own
// throttled_usec, which is arrived at by a completely different route.
func throttlingScenario() Scenario {
	const (
		quotaUsec  = 50000
		periodUsec = 100000
	)
	share := 1 - float64(quotaUsec)/float64(periodUsec)

	return Scenario{
		Name:        "cgroup throttling",
		Category:    "throttled",
		GroundTruth: fmt.Sprintf("cpu.max %d %d, so %d%% of a spinner's life is forbidden", quotaUsec, periodUsec, int(100*share)),
		Expected:    time.Duration(share * float64(window)),
		// Tight, because this is the one where the kernel and the tool are
		// measuring the same thing from two sides and phase 2 already got
		// them to agree to four decimal places. What slack there is covers
		// the period boundary the subject starts on.
		Low:  time.Duration(0.92 * share * float64(window)),
		High: time.Duration(1.05 * share * float64(window)),
		Run: func() (time.Duration, error) {
			cg, err := newCgroup(quotaUsec, periodUsec)
			if err != nil {
				return 0, err
			}
			defer func() { _ = cg.remove() }()

			var group *spinners
			var kernelSays time.Duration
			obs, err := observe("throttled", false, nil, func() error {
				var err error
				if group, err = startSpinners(1, "throttled"); err != nil {
					return err
				}
				if err := cg.admit(group.pids()[0]); err != nil {
					return err
				}
				time.Sleep(window)
				// Read before the spinner is killed, while the cgroup still
				// has a member: an empty cgroup keeps its counters, but
				// reading first removes any question of it.
				usec, err := cg.throttledUsec()
				if err != nil {
					return err
				}
				kernelSays = time.Duration(usec) * time.Microsecond //nolint:gosec // microseconds since this cgroup was created
				group.stop()
				return nil
			})
			if group != nil {
				group.stop()
			}
			if err != nil {
				return 0, err
			}

			reported := obs.sum(func(t offcpu.Thread) time.Duration { return t.Throttled })

			// The second opinion, and it is the reason this scenario is the
			// most trustworthy row in the table. The arithmetic above says
			// what a 50% quota must cost; cpu.stat says what the kernel
			// itself charged, by a route that shares no code with this tool.
			// Two sources that agree are worth more than either, and a
			// disagreement here is a defect rather than a tolerance
			// question -- so it is an error, not a wide band.
			if kernelSays == 0 {
				return 0, fmt.Errorf("cpu.stat reports no throttling at all for a "+
					"cgroup capped at %d%% of a CPU with a spinner in it", quotaUsec/(periodUsec/100))
			}
			if ratio := float64(reported) / float64(kernelSays); ratio < 0.9 || ratio > 1.1 {
				return reported, fmt.Errorf("wallclock reports %v throttled and the "+
					"kernel's own cpu.stat %v, a ratio of %.3f: the two are measuring "+
					"the same thing from different sides and should agree",
					reported, kernelSays, ratio)
			}
			return reported, nil
		},
	}
}

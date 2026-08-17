// Package overhead measures what this tool costs the machine it watches.
//
// Not against the ticket office, and the reason is arithmetic. That target's
// own documented run-to-run variance is 153 ms to 333 ms at the p99, and the
// effect being looked for here is a few per cent: the signal would be an
// order of magnitude under the noise, and any number that came out of it
// would be a coin toss dressed as a measurement.
//
// So the load is synthetic, stable and tunable: two threads passing a byte
// back and forth through a pipe, with a controllable amount of CPU work
// between round trips. Each round trip is two context switches, which is what
// this tool attaches to, and turning the work per trip up and down sweeps the
// event rate across orders of magnitude. The answer is then a curve rather
// than a single number, which is a much better answer -- a profiler that
// costs 1% at ten thousand switches a second and 30% at half a million has
// told you where it may be used.
package overhead

import (
	"fmt"
	"time"

	"github.com/FranciscoPLoureiro/wallclock/internal/offcpu"
)

// Load is one setting of the synthetic workload.
//
// Two knobs, because one does not span the range. Work turns a single pair's
// rate down and bottoms out at whatever a wakeup costs on the machine; Pairs
// multiplies past that. Reaching a rate where the tool starts to hurt is the
// point of the exercise, and one pair on this host cannot get there.
type Load struct {
	Pairs int
	Work  time.Duration
}

func (l Load) String() string {
	return fmt.Sprintf("%d x %v", l.Pairs, l.Work)
}

// Run is one measured stretch of the synthetic load.
type Run struct {
	Trips   int64
	Elapsed time.Duration
}

// Rate is round trips per second.
func (r Run) Rate() float64 {
	if r.Elapsed <= 0 {
		return 0
	}
	return float64(r.Trips) / r.Elapsed.Seconds()
}

// Point is one setting of the load, measured with and without the profiler
// attached.
type Point struct {
	// Load is the setting this point exercises.
	Load     Load
	Baseline Run
	// Control is a second unprofiled run, and it is the most important
	// column here. Cost is a difference between two runs, and a difference
	// between two runs of anything on a real machine is never zero: cores
	// change frequency, other work wakes up, the pipe buffer is warmer the
	// second time. Measuring two identical runs against each other says how
	// big that difference is when the answer is known to be nothing, and any
	// overhead smaller than it has not been measured -- it has been guessed.
	Control  Run
	Profiled Run
	// Drops is what the kernel side could not record during the profiled
	// run. An overhead figure without this beside it is half an answer: a
	// tool can always be made cheap by dropping the events it cannot afford.
	Drops offcpu.Drops
}

// SwitchesPerSecond is the event rate this point exercises, taken from the
// unprofiled run so that the axis does not move when the thing being measured
// changes.
//
// Two per round trip: one when the sender blocks waiting for the reply, one
// when the receiver is woken. Wakeups are a separate tracepoint this tool also
// attaches to, so the true event count is higher; this is the number that can
// be stated exactly rather than estimated, and it is proportional to the rest.
func (p Point) SwitchesPerSecond() float64 { return 2 * p.Baseline.Rate() }

// Cost is the share of throughput the profiler took, from 0 to 1.
func (p Point) Cost() float64 {
	if p.Baseline.Rate() <= 0 {
		return 0
	}
	return 1 - p.Profiled.Rate()/p.Baseline.Rate()
}

// Noise is how far two identical unprofiled runs differ, as a magnitude. It
// is the resolution of the method: a cost below it is not a measurement.
func (p Point) Noise() float64 {
	if p.Baseline.Rate() <= 0 {
		return 0
	}
	n := 1 - p.Control.Rate()/p.Baseline.Rate()
	if n < 0 {
		return -n
	}
	return n
}

// Measured reports whether the cost is larger than the noise, and therefore
// whether the cost column means anything at this point.
func (p Point) Measured() bool { return p.Cost() > p.Noise() }

// Measure sweeps the load across the given settings.
//
// Each setting is measured repeats times, alternating unprofiled and profiled
// runs, and the median cost is kept. Alternating matters: the machine drifts
// -- another process wakes, a core changes frequency -- and measuring all the
// baselines first and all the profiled runs afterwards would put that drift
// squarely into the difference. The median rather than the mean because one
// interrupted run should not decide the answer.
func Measure(loads []Load, per time.Duration, repeats int, onPoint func(Load)) ([]Point, error) {
	points := make([]Point, 0, len(loads))
	for _, load := range loads {
		if onPoint != nil {
			onPoint(load)
		}

		// Thrown away. The first run of anything pays for page faults, a
		// cold pipe buffer and threads the runtime has not created yet.
		if _, err := pingpongFor(per/4, load.Work, load.Pairs); err != nil {
			return nil, err
		}

		best := Point{Load: load}
		costs := make([]float64, 0, repeats)
		for range repeats {
			baseline, err := pingpongFor(per, load.Work, load.Pairs)
			if err != nil {
				return nil, err
			}
			// The control sits between the two, so that whatever drifts
			// over the length of a repeat drifts across the control as much
			// as across the profiled run. Putting it last would let it
			// measure only the tail.
			control, err := pingpongFor(per, load.Work, load.Pairs)
			if err != nil {
				return nil, err
			}
			profiled, drops, err := profiledPingpong(per, load)
			if err != nil {
				return nil, err
			}
			candidate := Point{
				Load: load, Baseline: baseline, Control: control,
				Profiled: profiled, Drops: drops,
			}
			costs = append(costs, candidate.Cost())
			// The kept run is the one whose cost is the median, so that the
			// rates printed beside it are the rates that produced the number
			// rather than an average of runs that never happened.
			if len(costs) == 1 {
				best = candidate
			} else if isMedian(candidate.Cost(), costs) {
				best = candidate
			}
		}
		points = append(points, best)
	}
	return points, nil
}

// isMedian reports whether c is the middle value of the costs seen so far.
func isMedian(c float64, costs []float64) bool {
	below, above := 0, 0
	for _, other := range costs {
		switch {
		case other < c:
			below++
		case other > c:
			above++
		}
	}
	return below <= len(costs)/2 && above <= len(costs)/2
}

// profiledPingpong runs the load with a profiling session attached to the
// whole machine.
//
// The whole machine on purpose: it is the expensive case and therefore the
// honest one. Filtering to a pid makes the programs return early for every
// thread nobody asked about, which is most of them, and quoting that number
// would be quoting the best case as though it were the cost.
func profiledPingpong(d time.Duration, load Load) (Run, offcpu.Drops, error) {
	session, err := offcpu.Open(0)
	if err != nil {
		return Run{}, offcpu.Drops{}, fmt.Errorf("open a profiling session: %w", err)
	}
	defer session.Close()

	// Opened, then measured. Loading and attaching costs tens of
	// milliseconds and happens once per session rather than once per event,
	// so counting it here would report a fixed cost as a per-event one and
	// the curve would be nonsense at the cheap end.
	run, err := pingpongFor(d, load.Work, load.Pairs)
	if err != nil {
		return Run{}, offcpu.Drops{}, err
	}
	drops, err := session.Drops()
	if err != nil {
		return Run{}, offcpu.Drops{}, err
	}
	return run, drops, nil
}

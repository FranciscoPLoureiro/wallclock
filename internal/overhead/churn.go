package overhead

import (
	"fmt"
	"os/exec"
	"sync"
	"time"

	"github.com/FranciscoPLoureiro/wallclock/internal/offcpu"
)

// Churn is one measurement of how much process turnover the tool survives.
//
// The context-switch rate is not what breaks this tool -- the sweep in this
// package runs it at a quarter of a million switches a second and it loses
// nothing, because the same few threads are switching and the map holding
// them stays the same size. What fills a map is *new* threads: every process
// that starts takes a slot, and a slot is only released when userspace reads
// the map and drains the ones that have exited. So the limit is threads per
// read, and the way to find it is to start a great many processes without
// reading in between.
type Churn struct {
	// Spawned is how many processes were started.
	Spawned int
	// Window is how long that took.
	Window time.Duration
	// Observed is how many threads the tool had a record of at the end.
	Observed int
	Drops    offcpu.Drops
}

// PerSecond is the churn rate this point exercised.
func (c Churn) PerSecond() float64 {
	if c.Window <= 0 {
		return 0
	}
	return float64(c.Spawned) / c.Window.Seconds()
}

// MeasureChurn starts count short-lived processes with a session open, reads
// once at the end, and reports what was lost.
//
// Reading once at the end is deliberate and is the worst case: a session that
// reads every ten seconds drains exited threads every ten seconds, so what
// matters is the turnover *between* reads rather than the rate. Anyone
// profiling a build server or a CI runner is in this case.
func MeasureChurn(count, parallel int) (Churn, error) {
	session, err := offcpu.Open(0)
	if err != nil {
		return Churn{}, fmt.Errorf("open a profiling session: %w", err)
	}
	defer session.Close()

	began := time.Now()
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		fail error
	)
	per := count / parallel
	for range parallel {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range per {
				// /bin/true is the shortest-lived process there is: it
				// execs, does nothing, and exits. Each one is a fresh entry
				// in the threads map that nothing has drained.
				if err := exec.Command("/bin/true").Run(); err != nil {
					mu.Lock()
					if fail == nil {
						fail = fmt.Errorf("spawning /bin/true: %w", err)
					}
					mu.Unlock()
					return
				}
			}
		}()
	}
	wg.Wait()
	window := time.Since(began)
	if fail != nil {
		return Churn{}, fail
	}

	threads, err := session.Threads()
	if err != nil {
		return Churn{}, err
	}
	drops, err := session.Drops()
	if err != nil {
		return Churn{}, err
	}
	return Churn{
		Spawned:  per * parallel,
		Window:   window,
		Observed: len(threads),
		Drops:    drops,
	}, nil
}

package overhead

import (
	"errors"
	"fmt"
	"runtime"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

// pingpongFor runs pairs independent ping-pongs for d, and reports how many
// round trips they managed between them.
//
// Two knobs, because one is not enough to reach both ends of the range. The
// work between trips turns a single pair's rate *down*, and it bottoms out at
// whatever a wakeup costs on this machine -- measured here at about 140 µs a
// round trip, which caps one pair at some fourteen thousand switches a
// second. Running several pairs at once is how the rate goes *up* past that,
// and reaching a rate where the tool starts to hurt is the whole point of the
// exercise.
func pingpongFor(d, work time.Duration, pairs int) (Run, error) {
	var (
		total   int64
		mu      sync.Mutex
		outer   sync.WaitGroup
		failure error
	)
	for range pairs {
		outer.Add(1)
		go func() {
			defer outer.Done()
			run, err := onePingpong(d, work)
			mu.Lock()
			defer mu.Unlock()
			if err != nil && failure == nil {
				failure = err
			}
			total += run.Trips
		}()
	}
	outer.Wait()
	if failure != nil {
		return Run{}, failure
	}

	// The nominal duration, not the measured one, and this is worth a
	// sentence because it halved the noise. Every pair loops until a
	// deadline d from its own start, so the window each of them measures is
	// d by construction. Wall clock around the whole thing also contains the
	// creation of two threads and two pipes per pair -- a hundred and
	// twenty-eight threads at the top of the sweep -- and that setup varies
	// far more from run to run than the thing being measured. Dividing by it
	// put the harness's own scheduling into the overhead figure.
	return Run{Trips: total, Elapsed: d}, nil
}

// onePingpong passes a byte between two threads for d.
//
// Two threads and a pair of pipes, each thread pinned to its own OS thread.
// The pinning is what makes this a context-switch benchmark rather than a
// goroutine benchmark: without it the Go runtime would notice two goroutines
// handing work to each other, keep them on one thread, and the scheduler this
// tool attaches to would never be involved.
//
// One round trip is two context switches -- the sender blocks on the read of
// the reply, the receiver is woken -- plus the wakeups that go with them.
func onePingpong(d, work time.Duration) (Run, error) {
	toB, err := newPipe()
	if err != nil {
		return Run{}, err
	}
	defer toB.close()
	toA, err := newPipe()
	if err != nil {
		return Run{}, err
	}
	defer toA.close()

	var (
		wg    sync.WaitGroup
		trips int64
		fail  error
		once  sync.Once
	)
	report := func(err error) {
		if err != nil {
			once.Do(func() { fail = err })
		}
	}

	// B echoes whatever it is given, until the pipe is closed under it.
	wg.Add(1)
	go func() {
		defer wg.Done()
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		var buf [1]byte
		for {
			n, err := readFull(toB.read, buf[:])
			if err != nil {
				report(err)
				return
			}
			if n == 0 {
				return // A closed its end: the run is over.
			}
			if err := writeFull(toA.write, buf[:]); err != nil {
				report(err)
				return
			}
		}
	}()

	// A drives, and its loop is the thing being timed.
	wg.Add(1)
	go func() {
		defer wg.Done()
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()

		var buf [1]byte
		deadline := time.Now().Add(d)
		for time.Now().Before(deadline) {
			if work > 0 {
				spinFor(work)
			}
			if err := writeFull(toB.write, buf[:]); err != nil {
				report(err)
				break
			}
			if n, err := readFull(toA.read, buf[:]); err != nil {
				report(err)
				break
			} else if n == 0 {
				break
			}
			trips++
		}
		// Closing the write end is how B is told to stop: its read returns
		// zero rather than blocking for ever.
		toB.closeWrite()
	}()

	began := time.Now()
	wg.Wait()
	elapsed := time.Since(began)

	if fail != nil {
		return Run{}, fail
	}
	return Run{Trips: trips, Elapsed: elapsed}, nil
}

// spinFor holds a CPU for d, which is how the event rate is turned down: the
// more work between round trips, the fewer round trips a second.
func spinFor(d time.Duration) {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) { //nolint:revive // holding a CPU is the point
	}
}

type pipe struct {
	read, write int
	closedWrite bool
}

func newPipe() (*pipe, error) {
	var fds [2]int
	if err := unix.Pipe2(fds[:], 0); err != nil {
		return nil, fmt.Errorf("pipe: %w", err)
	}
	return &pipe{read: fds[0], write: fds[1]}, nil
}

func (p *pipe) closeWrite() {
	if !p.closedWrite {
		p.closedWrite = true
		_ = unix.Close(p.write)
	}
}

func (p *pipe) close() {
	p.closeWrite()
	_ = unix.Close(p.read)
}

// readFull reads len(b) bytes, or reports end of stream.
func readFull(fd int, b []byte) (int, error) {
	for read := 0; read < len(b); {
		n, err := unix.Read(fd, b[read:])
		if err != nil {
			// Go's async preemption signal can land on a thread inside a
			// syscall; the runtime asks for SA_RESTART so it is rare, and a
			// retry costs nothing when it is not.
			if errors.Is(err, unix.EINTR) {
				continue
			}
			return read, fmt.Errorf("read: %w", err)
		}
		if n == 0 {
			return read, nil
		}
		read += n
	}
	return len(b), nil
}

func writeFull(fd int, b []byte) error {
	for written := 0; written < len(b); {
		n, err := unix.Write(fd, b[written:])
		if err != nil {
			if errors.Is(err, unix.EINTR) {
				continue
			}
			// The other end going away is how a run ends, not a failure.
			if errors.Is(err, unix.EPIPE) {
				return nil
			}
			return fmt.Errorf("write: %w", err)
		}
		written += n
	}
	return nil
}

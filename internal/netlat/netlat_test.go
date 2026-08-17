package netlat_test

import (
	"io"
	"net"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/FranciscoPLoureiro/wallclock/internal/kerneltest"
	"github.com/FranciscoPLoureiro/wallclock/internal/netlat"
)

// Two servers, two delays decided in advance, one client, and the question is
// whether the answers are told apart.
//
// The client is ordinary Go -- net.Dial, Write, Read -- and that is the point
// rather than a convenience. Phase 3 established that a Go client parks
// goroutines in the netpoller and blocks no thread, so the original plan for
// this phase, charging *blocked* time to a destination, would have measured
// nothing here at all. What is measured instead is the interval from sending
// to the first bytes coming back, which no thread has to stop for. If this
// test passes with a netpolled client, the reframing did what it claimed.
func TestTwoDestinationsAtTwoDelaysAreToldApart(t *testing.T) {
	kerneltest.Root(t)

	const (
		quickDelay = 20 * time.Millisecond
		slowDelay  = 80 * time.Millisecond
		exchanges  = 20
	)

	quick := startDelayedEcho(t, quickDelay)
	slow := startDelayedEcho(t, slowDelay)

	session, err := netlat.Open(0)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer session.Close()

	// Both conversations run at once, so neither can be helped by having the
	// machine to itself.
	var wg sync.WaitGroup
	for _, server := range []net.Listener{quick, slow} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := exchangeWith(server.Addr().String(), exchanges); err != nil {
				t.Errorf("talking to %s: %v", server.Addr(), err)
			}
		}()
	}
	wg.Wait()

	found, err := session.Destinations()
	if err != nil {
		t.Fatalf("destinations: %v", err)
	}

	quickStats, ok := statsForPort(found, portOf(t, quick))
	if !ok {
		t.Fatalf("no measurements against the %v server among %d destinations: %s",
			quickDelay, len(found), describe(found))
	}
	slowStats, ok := statsForPort(found, portOf(t, slow))
	if !ok {
		t.Fatalf("no measurements against the %v server among %d destinations: %s",
			slowDelay, len(found), describe(found))
	}

	t.Logf("%v server: %d exchanges, mean %v, max %v",
		quickDelay, quickStats.Count, quickStats.Mean(), quickStats.Max)
	t.Logf("%v server: %d exchanges, mean %v, max %v",
		slowDelay, slowStats.Count, slowStats.Mean(), slowStats.Max)

	// Every exchange has to be there. A destination that appears with two of
	// twenty exchanges would still separate the two servers while measuring
	// almost nothing, and the mean would be of whatever happened to land.
	for _, c := range []struct {
		name  string
		stats netlat.Stats
	}{{"quick", quickStats}, {"slow", slowStats}} {
		if c.stats.Count < exchanges*8/10 {
			t.Errorf("the %s server answered %d times and only %d were paired",
				c.name, exchanges, c.stats.Count)
		}
	}

	// The bands are one-sided, because the error is. A handler that sleeps
	// for 20 ms cannot reply in less, so the floor is nearly hard; above it
	// sit the connect, the scheduler and the server's own turn on a CPU.
	assertWithin(t, "quick", quickStats.Mean(), quickDelay, quickDelay+25*time.Millisecond)
	assertWithin(t, "slow", slowStats.Mean(), slowDelay, slowDelay+25*time.Millisecond)

	// And the whole point: they are separated, not merely both present.
	if ratio := float64(slowStats.Mean()) / float64(quickStats.Mean()); ratio < 2.5 {
		t.Errorf("the %v server and the %v server came out at a ratio of %.2f: "+
			"the attribution is mixing them", slowDelay, quickDelay, ratio)
	}

	drops, err := session.Drops()
	if err != nil {
		t.Fatalf("drops: %v", err)
	}
	if drops.Any() {
		t.Errorf("lost measurements: %d destinations full, %d threads full",
			drops.DestinationsFull, drops.ThreadsFull)
	}
}

// assertWithin fails when d is outside [low, high], saying by how much.
func assertWithin(t *testing.T, name string, d, low, high time.Duration) {
	t.Helper()
	if d < low || d > high {
		t.Errorf("the %s server measured %v, outside the declared %v to %v",
			name, d, low, high)
	}
}

// startDelayedEcho answers every request after exactly delay, and not before.
func startDelayedEcho(t *testing.T, delay time.Duration) net.Listener {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { listener.Close() })

	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				if tcp, ok := conn.(*net.TCPConn); ok {
					// Otherwise the kernel may hold a small reply back
					// waiting for more to send, adding a delay this test
					// has not accounted for and cannot see.
					_ = tcp.SetNoDelay(true)
				}
				buf := make([]byte, 32)
				for {
					n, err := conn.Read(buf)
					if err != nil {
						return
					}
					time.Sleep(delay)
					if _, err := conn.Write(buf[:n]); err != nil {
						return
					}
				}
			}()
		}
	}()
	return listener
}

// exchangeWith does n request-and-answer round trips on one connection.
//
// One connection, reused, because opening a new one per exchange would put a
// TCP handshake inside every measurement and the expected value would stop
// being the server's delay.
func exchangeWith(addr string, n int) error {
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		return err
	}
	defer conn.Close()
	if tcp, ok := conn.(*net.TCPConn); ok {
		_ = tcp.SetNoDelay(true)
	}

	request := []byte("wallclock")
	reply := make([]byte, len(request))
	for range n {
		if _, err := conn.Write(request); err != nil {
			return err
		}
		if _, err := io.ReadFull(conn, reply); err != nil {
			return err
		}
	}
	return nil
}

func portOf(t *testing.T, l net.Listener) uint16 {
	t.Helper()
	_, port, err := net.SplitHostPort(l.Addr().String())
	if err != nil {
		t.Fatalf("split %q: %v", l.Addr(), err)
	}
	n, err := strconv.ParseUint(port, 10, 16)
	if err != nil {
		t.Fatalf("port %q: %v", port, err)
	}
	return uint16(n)
}

// statsForPort picks one server out of everything the window recorded.
//
// Filtering is necessary and says something about the tool: both ends of
// these conversations are in this process, so the servers' own replies are
// measured too, against the client's ephemeral ports. A destination, seen
// from a server, is a client.
func statsForPort(all []netlat.Stats, port uint16) (netlat.Stats, bool) {
	for _, s := range all {
		if s.Destination.Port == port {
			return s, true
		}
	}
	return netlat.Stats{}, false
}

func describe(all []netlat.Stats) string {
	out := ""
	for i, s := range all {
		if i > 0 {
			out += ", "
		}
		out += s.Destination.String() + " x" + strconv.FormatUint(s.Count, 10)
	}
	if out == "" {
		return "(none)"
	}
	return out
}

// A destination nobody has spoken to has no percentile, and asking for one
// must not divide by zero.
func TestEmptyStatsAreQuiet(t *testing.T) {
	var s netlat.Stats
	if got := s.Mean(); got != 0 {
		t.Errorf("mean of nothing is %v", got)
	}
	if got := s.Percentile(0.99); got != 0 {
		t.Errorf("p99 of nothing is %v", got)
	}
}

// The percentile is read off a log2 histogram, so it is an upper bound rather
// than an interpolation, and the test says which.
func TestPercentileReturnsTheBucketCeiling(t *testing.T) {
	var s netlat.Stats
	// 90 samples in the 8-16us bucket, 10 in the 1024-2048us bucket.
	s.Buckets[3] = 90
	s.Buckets[10] = 10
	s.Count = 100

	if got, want := s.Percentile(0.5), 16*time.Microsecond; got != want {
		t.Errorf("p50 = %v, want %v: the ceiling of the bucket holding the median", got, want)
	}
	if got, want := s.Percentile(0.99), 2048*time.Microsecond; got != want {
		t.Errorf("p99 = %v, want %v: the ceiling of the bucket holding the tail", got, want)
	}
}

// Unpaired answers are not a loss, and Any must not claim they are.
func TestUnpairedAnswersAreNotCountedAsLoss(t *testing.T) {
	if (netlat.Drops{Unpaired: 1000}).Any() {
		t.Error("unpaired answers reported as lost: they describe the traffic, " +
			"they are not a failure to record it")
	}
	if !(netlat.Drops{ThreadsFull: 1}).Any() {
		t.Error("a full threads map is a real loss and was not reported")
	}
}

package offcpu_test

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strconv"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/FranciscoPLoureiro/wallclock/internal/kerneltest"
	"github.com/FranciscoPLoureiro/wallclock/internal/ksyms"
	"github.com/FranciscoPLoureiro/wallclock/internal/offcpu"
)

const (
	// The delay the server holds every request for. This is ground truth:
	// nothing here measures how long a socket really took, it measures
	// whether a wait of a length already known is visible from the kernel.
	netpollDelay = 40 * time.Millisecond
	// Eight clients of twenty-five requests each -- eight seconds of waiting
	// inside about a second of wall clock, and the same eight seconds in both
	// modes. Making the two windows equal is the whole design: what differs
	// between them is not the amount of waiting, it is who does it.
	netpollClients   = 8
	netpollPerClient = 25

	anchorName   = testThreadPrefix + "anchor"
	blockingName = testThreadPrefix + "blkclient"
)

// netpollTotalWait is the waiting the workload is built to contain, in either
// mode.
const netpollTotalWait = netpollClients * netpollPerClient * netpollDelay

// The measurement this whole phase exists for.
//
// A Go program's network waiting is done by goroutines, and goroutines are
// not threads. The runtime puts every network file descriptor in non-blocking
// mode and hands it to an epoll instance; a goroutine that reads from a socket
// with nothing in it is parked, its OS thread immediately picks up another
// goroutine, and no thread is left waiting anywhere. From the kernel there is
// nothing to see, because from the kernel nothing happened.
//
// That is a claim about a negative, and a negative measured with one tool is
// worthless: "wallclock reports no network waiting in a Go service" and
// "wallclock cannot see network waiting" produce the same output. So the same
// binary, against the same server, holding every request for the same known
// delay, does the same eight seconds of waiting twice:
//
//   - once through net/http, where the runtime owns the descriptor and parks
//     goroutines on it;
//   - once through raw blocking read() on a socket the runtime has never
//     touched, on threads pinned so that the thread really is the thing that
//     stops.
//
// The second run is the control. It proves the tool sees thread-level network
// waiting when there is thread-level network waiting to see, which is what
// makes the first run's near-zero a fact about Go rather than a fact about
// this profiler.
func TestGoroutineWaitsAreInvisibleAndThreadWaitsAreNot(t *testing.T) {
	kerneltest.Root(t)

	server := slowServer(t)
	defer server.Close()

	byGoroutine, pool := measureBlockedReasons(t, func() {
		runNetpollClients(t, server.URL)
	})
	byThread, _ := measureBlockedReasons(t, func() {
		runBlockingClients(t, server.Listener.Addr().String())
	})

	t.Logf("the same %v of waiting, done twice:", netpollTotalWait)
	t.Logf("  through the netpoller: %s", byGoroutine)
	t.Logf("  on blocked threads:    %s", byThread)
	// The two totals, because the interesting thing about them is that they
	// are the same. Neither run waits more than the other and neither has
	// more threads; what moves between them is which column the waiting is
	// filed under. Logged rather than asserted: how much of it sits in futex
	// depends on how many idle Ms the runtime keeps, which depends on
	// GOMAXPROCS, which depends on the machine.
	t.Logf("  total blocked, netpoller %v against threads %v",
		byGoroutine.total().Round(time.Millisecond),
		byThread.total().Round(time.Millisecond))

	// The control first, because if it fails the other assertion means
	// nothing. Two thirds rather than all of it: a request also connects,
	// writes and is scheduled, the client threads are only observed from
	// their first switch onwards, and the server has to be given a CPU too.
	// The claim being defended is an order of magnitude, so the bound is set
	// where it still is one.
	const floor = netpollTotalWait * 2 / 3
	if byThread.network < floor {
		t.Fatalf("the control failed: %v of network blocking on threads that "+
			"really did block, out of %v of waiting, below the %v this test "+
			"needs before it can conclude anything from the other run",
			byThread.network, netpollTotalWait, floor)
	}

	// And the finding. A tenth of the control is not a rounding tolerance --
	// it is the size of the effect this phase reports, and anything under it
	// would still be the same conclusion.
	ceiling := byThread.network / 10
	if byGoroutine.network > ceiling {
		t.Errorf("goroutine waiting shows %v of network blocking against the "+
			"control's %v: the runtime is not parking these waits in the "+
			"netpoller, or something else on this machine is being counted",
			byGoroutine.network, byThread.network)
	}

	// Where it went instead. Not an assertion about a number -- it is the
	// explanation, and if the time is not in the poller then the account
	// given in the README is wrong even when the numbers above are right.
	if byGoroutine.poll <= byGoroutine.network {
		t.Errorf("the netpoller run shows %v in the poller against %v on "+
			"sockets: the waiting is not where the explanation says it is",
			byGoroutine.poll, byGoroutine.network)
	}

	// And the shape, against the classifier that has to recognise it in the
	// field. This process is a Go runtime, so it had better look like one:
	// the polling spread over a pool of threads with none of them owning it.
	// Asserting it here rather than only in the table-driven tests is what
	// stops those tables from drifting into describing a Go runtime that
	// stopped existing three releases ago.
	t.Logf("  the netpoller run as a pool: %d of %d threads polled, %.0f%% of the "+
		"polling on dedicated threads, busiest held %.1f%%",
		pool.Polling, pool.Threads, 100*pool.Dedication, 100*pool.Busiest)
	if !pool.Rotating() {
		t.Errorf("a Go runtime under load was not recognised as a rotating "+
			"poller: %d of %d threads polled, %.0f%% of the polling was done "+
			"by threads dedicated to it, and the busiest held %.1f%%",
			pool.Polling, pool.Threads, 100*pool.Dedication, 100*pool.Busiest)
	}
}

// reasonTotals is one window's blocked time, split by what was waited on, for
// one process.
type reasonTotals struct {
	network time.Duration
	poll    time.Duration
	futex   time.Duration
	sleep   time.Duration
	disk    time.Duration
	other   time.Duration
	threads int
}

// total is every category added up: the blocked time of the whole process
// over the window, arrived at by the reasons rather than by the thread rows.
func (r reasonTotals) total() time.Duration {
	return r.network + r.poll + r.futex + r.sleep + r.disk + r.other
}

func (r reasonTotals) String() string {
	return fmt.Sprintf("network %v, poll %v, futex %v, sleep %v, disk %v, other %v, over %d threads",
		r.network.Round(time.Millisecond), r.poll.Round(time.Millisecond),
		r.futex.Round(time.Millisecond), r.sleep.Round(time.Millisecond),
		r.disk.Round(time.Millisecond), r.other.Round(time.Millisecond), r.threads)
}

// measureBlockedReasons profiles one run of workload and totals the blocked
// time of every thread of this process, by reason.
//
// Every thread of the process, not the ones that look interesting. In the
// netpoller run there is no interesting thread to find -- that is the result
// -- so the sum has to be over the whole process or it would be measuring the
// absence of something nobody looked for.
func measureBlockedReasons(t *testing.T, workload func()) (reasonTotals, offcpu.Poller) {
	t.Helper()

	session, err := offcpu.Open(0)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer session.Close()

	stop := startAnchor(t)
	workload()
	stop()

	symbols, err := ksyms.Load()
	if err != nil {
		t.Fatalf("load kernel symbols: %v", err)
	}
	// Reasons before threads, then the open waits: reading them the other way
	// round counts an interval that ends in between twice. See BlockedReasons.
	blocked, err := session.BlockedReasons(symbols)
	if err != nil {
		t.Fatalf("blocked reasons: %v", err)
	}
	threads, err := session.Threads()
	if err != nil {
		t.Fatalf("threads: %v", err)
	}
	blocked, err = session.AttributeOpenWaits(blocked, threads, symbols)
	if err != nil {
		t.Fatalf("attribute open waits: %v", err)
	}

	anchor, ok := findByComm(threads, anchorName)
	if !ok {
		t.Fatalf("no thread named %q among %d observed: the window saw nothing "+
			"of this process", anchorName, len(threads))
	}
	if anchor.TGID == 0 {
		t.Fatalf("the anchor thread %d reports no process, so the threads of "+
			"this process cannot be told from anybody else's", anchor.TID)
	}

	ours := make(map[uint32]struct{})
	for _, thread := range threads {
		if thread.TGID == anchor.TGID {
			ours[thread.TID] = struct{}{}
		}
	}

	totals := reasonTotals{threads: len(ours)}
	for _, b := range blocked {
		if _, mine := ours[b.TID]; !mine {
			continue
		}
		switch b.Reason {
		case offcpu.ReasonNetwork:
			totals.network += b.Duration
		case offcpu.ReasonPoll:
			totals.poll += b.Duration
		case offcpu.ReasonFutex:
			totals.futex += b.Duration
		case offcpu.ReasonSleep:
			totals.sleep += b.Duration
		case offcpu.ReasonDisk:
			totals.disk += b.Duration
		case offcpu.ReasonOther:
			totals.other += b.Duration
		}
	}
	return totals, offcpu.Pollers(blocked, threads)[anchor.TGID]
}

// startAnchor pins and names one thread so the process can be recognised.
//
// The kernel numbers processes in the initial namespace and this test may not
// be in it, so getpid() is not an answer. A named thread is the same string on
// both sides, and the process it reports is this one by construction.
func startAnchor(t *testing.T) func() {
	t.Helper()
	done := make(chan struct{})
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		nameThisThread(t, anchorName)
		defer runtime.UnlockOSThread()
		for {
			select {
			case <-done:
				return
			case <-time.After(10 * time.Millisecond):
			}
		}
	}()
	return func() {
		close(done)
		<-stopped
	}
}

// slowServer answers every request after netpollDelay, and not before.
func slowServer(t *testing.T) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			time.Sleep(netpollDelay)
			_, _ = w.Write([]byte("ok"))
		}))
	return server
}

// runNetpollClients does the waiting the way a Go program does it.
func runNetpollClients(t *testing.T, url string) {
	t.Helper()
	// Keep-alives off, so that this mode opens exactly as many connections as
	// the raw one does and the two runs differ in one thing only.
	client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
	defer client.CloseIdleConnections()

	var wg sync.WaitGroup
	for range netpollClients {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range netpollPerClient {
				resp, err := client.Get(url)
				if err != nil {
					t.Errorf("netpoller client: %v", err)
					return
				}
				_, _ = io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
			}
		}()
	}
	wg.Wait()
}

// runBlockingClients does the same waiting on threads that really stop.
//
// Raw syscalls on a descriptor the runtime has never seen. net.Dial would put
// it in non-blocking mode and register it with the netpoller, which is exactly
// the behaviour being controlled for, so the socket is opened, connected and
// read through syscall directly. LockOSThread is what makes the measurement
// mean anything: without it the runtime is free to move the goroutine, and the
// wait would be spread over whichever threads it visited.
func runBlockingClients(t *testing.T, addr string) {
	t.Helper()
	sa := sockaddrOf(t, addr)
	request := []byte("GET / HTTP/1.1\r\nHost: wallclock\r\nConnection: close\r\n\r\n")

	var wg sync.WaitGroup
	for i := range netpollClients {
		wg.Add(1)
		go func() {
			defer wg.Done()
			nameThisThread(t, blockingName+strconv.Itoa(i))
			defer runtime.UnlockOSThread()
			for range netpollPerClient {
				if err := blockingRequest(sa, request); err != nil {
					t.Errorf("blocking client %d: %v", i, err)
					return
				}
			}
		}()
	}
	wg.Wait()
}

func blockingRequest(sa *syscall.SockaddrInet4, request []byte) error {
	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_STREAM, 0)
	if err != nil {
		return fmt.Errorf("socket: %w", err)
	}
	defer syscall.Close(fd)

	if err := syscall.Connect(fd, sa); err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	for written := 0; written < len(request); {
		n, err := syscall.Write(fd, request[written:])
		if err != nil {
			if err == syscall.EINTR {
				continue
			}
			return fmt.Errorf("write: %w", err)
		}
		written += n
	}

	// Read to end of stream. The server was asked to close, so this returns
	// zero when the whole answer has arrived -- and until the answer starts
	// arriving, this thread is stopped inside the kernel, which is the point.
	buf := make([]byte, 4096)
	for {
		n, err := syscall.Read(fd, buf)
		if err != nil {
			if err == syscall.EINTR {
				continue
			}
			return fmt.Errorf("read: %w", err)
		}
		if n == 0 {
			return nil
		}
	}
}

func sockaddrOf(t *testing.T, addr string) *syscall.SockaddrInet4 {
	t.Helper()
	host, portText, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("split %q: %v", addr, err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatalf("port of %q: %v", addr, err)
	}
	ip := net.ParseIP(host).To4()
	if ip == nil {
		t.Fatalf("%q is not an IPv4 address", host)
	}
	sa := &syscall.SockaddrInet4{Port: port}
	copy(sa.Addr[:], ip)
	return sa
}

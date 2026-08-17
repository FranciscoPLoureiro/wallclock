package validate

import (
	"errors"
	"fmt"
	"net"
	"os/exec"
	"strconv"
	"syscall"
	"time"

	"github.com/FranciscoPLoureiro/wallclock/internal/offcpu"
)

// The injected delay, and how many round trips pay it.
//
// Applied to the loopback device, where it is charged in *both* directions:
// a request is delayed on its way out and the answer is delayed on its way
// back, so one round trip costs twice the configured delay. Measured before
// this was written rather than assumed -- `ping 127.0.0.1` under
// `netem delay 50ms` reports an RTT of 100.6 ms. Expecting 50 ms would have
// produced a table showing 100% error and reading like a defect in the tool.
const (
	netemDelay  = 50 * time.Millisecond
	netemRTT    = 2 * netemDelay
	netemTrips  = 25
	netemDevice = "lo"
)

// netemScenario: a socket with a known delay on it, and a thread that really
// waits for it.
//
// This is the scenario phase 3 constrained. Injected network latency can only
// be validated against something that stops a *thread*, and almost nothing
// does: a Go client parks a goroutine in the netpoller, and PostgreSQL and
// Redis wait in epoll. So the subject here is a raw blocking socket on a
// pinned thread -- the one shape that turns a network delay into blocked
// time the kernel can see.
func netemScenario() Scenario {
	// One round trip per exchange, plus the connect: a blocking connect
	// returns when the SYN-ACK arrives, which is a round trip of its own and
	// is charged to the same category by the same stack.
	const trips = netemTrips + 1

	return Scenario{
		Name:     "network delay",
		Category: "blocked (network)",
		GroundTruth: fmt.Sprintf("netem %v on %s, charged both ways, over %d round trips and a connect",
			netemDelay, netemDevice, netemTrips),
		Expected: trips * netemRTT,
		// Loose above and tight below, because the error has a direction.
		// Every round trip must cost at least the injected delay -- netem
		// cannot deliver early -- so the floor is nearly hard. Above it sit
		// the scheduler, the server's own turn on a CPU, and netem's timer
		// granularity, all of which add.
		Low:  time.Duration(0.97 * float64(trips*netemRTT)),
		High: time.Duration(1.15 * float64(trips*netemRTT)),
		Needs: func() error {
			if _, err := exec.LookPath("tc"); err != nil {
				return errors.New("tc is not installed, so no delay can be injected")
			}
			// Attempted rather than inspected, which is how every other
			// requirement in this repository is checked: the capability is
			// the operation succeeding, not a file existing somewhere.
			if err := addNetem(netemDelay); err != nil {
				return fmt.Errorf("cannot add a netem qdisc to %s: %w", netemDevice, err)
			}
			return delNetem()
		},
		Run: func() (time.Duration, error) {
			if err := addNetem(netemDelay); err != nil {
				return 0, err
			}
			defer func() { _ = delNetem() }()

			server, err := startEchoServer()
			if err != nil {
				return 0, err
			}
			defer server.Close()

			obs, err := observe("netem", true, nil, func() error {
				done := make(chan error, 1)
				go func() { done <- blockingRoundTrips(server.Addr().String(), netemTrips) }()
				return <-done
			})
			if err != nil {
				return 0, err
			}

			// A shortfall here has two explanations that the headline number
			// cannot tell apart: the waiting was filed under another reason,
			// or it was never seen. So when network is not nearly all of
			// what this thread waited on, the breakdown goes in the message
			// rather than leaving the next reader to guess.
			network := obs.blockedFor(offcpu.ReasonNetwork)
			blocked := obs.sum(func(t offcpu.Thread) time.Duration { return t.Blocked })
			if blocked > 0 && float64(network)/float64(blocked) < 0.9 {
				return network, fmt.Errorf("only %v of this thread's blocked time was "+
					"filed as network: %s", network, obs.describe())
			}
			return network, nil
		},
	}
}

// addNetem puts a fixed delay on every packet the loopback device carries.
//
// The whole device, because that is the only handle netem offers and the
// alternative -- classful filtering on a port -- adds a qdisc hierarchy whose
// correctness would itself need validating. It is in place for about three
// seconds and is removed by a defer on every path out.
func addNetem(delay time.Duration) error {
	return runTC("add", "dev", netemDevice, "root", "netem", "delay",
		strconv.FormatInt(delay.Milliseconds(), 10)+"ms")
}

// delNetem takes it off again. Called from a defer, and also as the second
// half of the capability check.
func delNetem() error {
	return runTC("del", "dev", netemDevice, "root")
}

func runTC(args ...string) error {
	// Every argument reaching here is a constant of this file or a duration
	// formatted as digits; nothing comes from a flag, a file or the network.
	cmd := exec.Command("tc", append([]string{"qdisc"}, args...)...) //nolint:gosec // see above
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("tc qdisc %v: %w: %s", args, err, out)
	}
	return nil
}

// startEchoServer answers every byte it is sent, on loopback.
//
// An ordinary Go listener, so the server side contributes nothing to the
// measurement: the runtime puts its descriptors in the netpoller and no
// thread of it ever waits on a socket. That is phase 3's finding being used
// rather than worked around -- the server cannot pollute a network-blocked
// total because it is incapable of producing one.
func startEchoServer() (net.Listener, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("listening on loopback: %w", err)
	}
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				if tcp, ok := conn.(*net.TCPConn); ok {
					// Without this the kernel may hold a small reply back
					// waiting for more to send, which would add a delay this
					// scenario has not accounted for and cannot see.
					_ = tcp.SetNoDelay(true)
				}
				buf := make([]byte, 64)
				for {
					n, err := conn.Read(buf)
					if err != nil {
						return
					}
					if _, err := conn.Write(buf[:n]); err != nil {
						return
					}
				}
			}()
		}
	}()
	return listener, nil
}

// blockingRoundTrips exchanges messages on a socket the Go runtime has never
// touched, from a thread pinned so that the thread really is what stops.
//
// syscall.Socket rather than net.Dial, and the difference is the entire
// point: net.Dial would set the descriptor non-blocking and register it with
// the netpoller, which is exactly the behaviour that makes network waiting
// invisible. Here nothing is registered anywhere, so read() stops the thread
// and the kernel has something to report.
func blockingRoundTrips(addr string, trips int) error {
	unpin, err := nameThisThread("netem")
	if err != nil {
		return err
	}
	defer unpin()

	sa, err := sockaddrOf(addr)
	if err != nil {
		return err
	}

	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_STREAM, 0)
	if err != nil {
		return fmt.Errorf("socket: %w", err)
	}
	defer syscall.Close(fd)
	if err := syscall.SetsockoptInt(fd, syscall.IPPROTO_TCP, syscall.TCP_NODELAY, 1); err != nil {
		return fmt.Errorf("TCP_NODELAY: %w", err)
	}
	if err := syscall.Connect(fd, sa); err != nil {
		return fmt.Errorf("connect: %w", err)
	}

	message := []byte("wallclock")
	buf := make([]byte, len(message))
	for range trips {
		if err := writeFull(fd, message); err != nil {
			return err
		}
		if err := readFull(fd, buf); err != nil {
			return err
		}
	}
	return nil
}

func writeFull(fd int, b []byte) error {
	for written := 0; written < len(b); {
		n, err := syscall.Write(fd, b[written:])
		if err != nil {
			// Go's async preemption signal can land on a thread inside a
			// syscall. The runtime installs its handlers with SA_RESTART so
			// this is rare, and a retry costs nothing when it is not.
			if errors.Is(err, syscall.EINTR) {
				continue
			}
			return fmt.Errorf("write: %w", err)
		}
		written += n
	}
	return nil
}

func readFull(fd int, b []byte) error {
	for read := 0; read < len(b); {
		n, err := syscall.Read(fd, b[read:])
		if err != nil {
			if errors.Is(err, syscall.EINTR) {
				continue
			}
			return fmt.Errorf("read: %w", err)
		}
		if n == 0 {
			return errors.New("the server closed the connection early")
		}
		read += n
	}
	return nil
}

func sockaddrOf(addr string) (*syscall.SockaddrInet4, error) {
	host, portText, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("splitting %q: %w", addr, err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		return nil, fmt.Errorf("port of %q: %w", addr, err)
	}
	ip := net.ParseIP(host).To4()
	if ip == nil {
		return nil, fmt.Errorf("%q is not an IPv4 address", host)
	}
	sa := &syscall.SockaddrInet4{Port: port}
	copy(sa.Addr[:], ip)
	return sa, nil
}

package offcpu_test

import (
	"os/exec"
	"testing"
	"time"

	"github.com/FranciscoPLoureiro/wallclock/internal/kerneltest"
	"github.com/FranciscoPLoureiro/wallclock/internal/ksyms"
	"github.com/FranciscoPLoureiro/wallclock/internal/offcpu"
)

// Classification is a pure function over frame names, so most of it can be
// tested without a kernel at all -- and the stacks below are real ones, taken
// from the folded output of a run rather than invented.
func TestClassify(t *testing.T) {
	cases := []struct {
		name   string
		frames []string
		want   offcpu.Reason
	}{
		{
			name: "a socket read",
			frames: []string{
				"__schedule", "schedule", "schedule_timeout",
				"__skb_wait_for_more_packets", "__skb_recv_datagram",
				"skb_recv_datagram", "ping_recvmsg", "inet_recvmsg",
				"sock_recvmsg", "__sys_recvmsg", "do_syscall_64",
			},
			want: offcpu.ReasonNetwork,
		},
		{
			name: "a contended mutex",
			frames: []string{
				"__schedule", "schedule", "futex_wait_queue", "futex_wait",
				"do_futex", "__x64_sys_futex", "do_syscall_64",
			},
			want: offcpu.ReasonFutex,
		},
		{
			name: "waiting on a disk",
			frames: []string{
				"__schedule", "schedule", "io_schedule", "folio_wait_bit_common",
				"filemap_read", "vfs_read", "do_syscall_64",
			},
			want: offcpu.ReasonDisk,
		},
		{
			name: "a deliberate pause",
			frames: []string{
				"__schedule", "schedule", "do_nanosleep", "hrtimer_nanosleep",
				"__x64_sys_clock_nanosleep", "do_syscall_64",
			},
			want: offcpu.ReasonSleep,
		},
		{
			name: "parked in epoll, which is not the same as slow network",
			frames: []string{
				"__schedule", "schedule", "schedule_hrtimeout_range",
				"ep_poll", "do_epoll_wait", "__x64_sys_epoll_pwait",
			},
			want: offcpu.ReasonPoll,
		},
		{
			name: "a kernel thread with nothing to do",
			frames: []string{
				"__schedule", "schedule", "rcu_gp_kthread", "kthread",
				"ret_from_fork",
			},
			want: offcpu.ReasonOther,
		},
		{
			name:   "nothing at all",
			frames: nil,
			want:   offcpu.ReasonOther,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := offcpu.Classify(tc.frames); got != tc.want {
				t.Errorf("Classify() = %q, want %q", got, tc.want)
			}
		})
	}
}

// The scheduler frames sit on top of every wait there is, so a classifier
// that looked only at the innermost frame would answer "schedule" for
// everything. This is the case that would have caught that.
func TestClassifyLooksPastTheSchedulerFrames(t *testing.T) {
	frames := []string{"__schedule", "schedule", "schedule_timeout", "tcp_recvmsg"}
	if got := offcpu.Classify(frames); got != offcpu.ReasonNetwork {
		t.Errorf("Classify() = %q, want %q -- the scheduler frames were not looked past",
			got, offcpu.ReasonNetwork)
	}
}

// End to end: a process that blocks on a socket must be reported as blocked
// on a socket, with a real stack behind the label.
func TestABlockedSocketReadIsClassifiedAsNetwork(t *testing.T) {
	kerneltest.Root(t)

	symbols, err := ksyms.Load()
	if err != nil {
		t.Skipf("kernel symbols are unavailable, so stacks cannot be named: %v", err)
	}

	session, err := offcpu.Open(0)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer session.Close()

	// ping waits in the kernel's socket receive path between replies, which
	// is a real network wait rather than a simulated one.
	cmd := exec.Command("ping", "-i", "0.2", "-c", "6", "127.0.0.1")
	if err := cmd.Run(); err != nil {
		t.Skipf("ping is unavailable or refused here: %v", err)
	}
	// Single-threaded, so its tid is its pid, and it is the subject this test
	// is entitled to make claims about.
	pingTID := uint32(cmd.Process.Pid) //nolint:gosec // a pid fits

	time.Sleep(50 * time.Millisecond)

	blocked, err := session.BlockedReasons(symbols)
	if err != nil {
		t.Fatalf("blocked reasons: %v", err)
	}
	threads, err := session.Threads()
	if err != nil {
		t.Fatalf("threads: %v", err)
	}
	if blocked, err = session.AttributeOpenWaits(blocked, threads, symbols); err != nil {
		t.Fatalf("attributing open waits: %v", err)
	}

	// The subject is identified by tid, not by hoping a distinctive symbol
	// turns up in the stack. The earlier version of this test required a
	// frame called ping_recvmsg or inet_recvmsg, and both of those are
	// choices the kernel build makes rather than facts about the wait: the
	// inet_recvmsg frame is on the captured stack on 6.10 and not on 7.1,
	// with the same ping and the same guest, while the symbol itself is in
	// kallsyms on both -- and which of the two names appears at all depends
	// on whether ping got a raw socket or an ICMP datagram socket, which
	// depends on whether it was running as root. The
	// test then failed on a kernel where the classification was perfectly
	// correct -- reported network, 100% of the wait -- because a static
	// function had been inlined. Matching the tid is both stricter about
	// whose wait this is and indifferent to what the compiler did.
	var networkStack []string
	var reasons []offcpu.Reason
	for _, b := range blocked {
		if b.TID != pingTID {
			continue
		}
		reasons = append(reasons, b.Reason)
		if b.Reason != offcpu.ReasonNetwork {
			continue
		}
		for _, frame := range b.Stack {
			// sock_recvmsg is the exported entry to the socket receive path
			// and cannot be inlined out of existence; the rest are accepted
			// because any of them is already inside it.
			switch frame {
			case "sock_recvmsg", "inet_recvmsg", "ping_recvmsg", "raw_recvmsg",
				"udp_recvmsg", "skb_recv_datagram", "__skb_wait_for_more_packets":
				networkStack = b.Stack
			}
		}
	}
	if networkStack == nil {
		t.Fatalf("ping (tid %d) was not reported blocked in a socket read; across "+
			"%d blocked intervals it was seen waiting for %v", pingTID, len(blocked), reasons)
	}
	t.Logf("stack: %v", networkStack)
}

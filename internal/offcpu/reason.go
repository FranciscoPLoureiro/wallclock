package offcpu

import "strings"

// Reason is why a thread stopped, read off the kernel stack it stopped on.
//
// "Blocked" on its own is half an answer. A thread waiting on a socket, a
// thread waiting on a mutex and a thread waiting on a disk are the same event
// to the scheduler and three different problems to whoever has to fix one, so
// the report names which.
type Reason string

// The categories, and the residual. Other is not a failure of the
// classifier — it is a real answer, it is displayed, and what falls into it
// is information rather than dirt to be swept up.
const (
	ReasonNetwork Reason = "network"
	ReasonFutex   Reason = "futex"
	ReasonDisk    Reason = "disk"
	ReasonSleep   Reason = "sleep"
	// ReasonPoll is a thread parked in epoll or select, waiting to be told
	// that something is ready.
	//
	// Separate from network on purpose, and the reason is the whole of phase
	// 3. A Go runtime parks its goroutines in a netpoller and the OS thread
	// sits in epoll_wait; that thread is idle, not slow, and folding it into
	// "network" would report a busy server as spending its life waiting on
	// sockets. It is waiting to be given work.
	ReasonPoll  Reason = "poll"
	ReasonOther Reason = "other"
)

// reasonPatterns maps a substring of a kernel symbol to what it means.
//
// Matched against the stack from the innermost frame outwards, first hit
// wins, so the ordering within a stack does the work rather than the ordering
// of this table. The generic scheduler frames -- schedule, __schedule,
// schedule_timeout -- are deliberately absent: they sit on top of every wait
// there is and say nothing about which one, so a stack is scanned past them
// until something meaningful appears.
var reasonPatterns = []struct {
	needle string
	reason Reason
}{
	{"futex", ReasonFutex},
	{"rwsem", ReasonFutex},
	{"mutex_lock", ReasonFutex},

	{"io_schedule", ReasonDisk},
	{"blk_", ReasonDisk},
	{"wait_on_page", ReasonDisk},
	{"folio_wait", ReasonDisk},
	{"jbd2", ReasonDisk},
	{"submit_bio", ReasonDisk},

	{"tcp_", ReasonNetwork},
	{"inet_", ReasonNetwork},
	{"sk_wait", ReasonNetwork},
	{"sock_", ReasonNetwork},
	{"unix_stream", ReasonNetwork},
	{"unix_dgram", ReasonNetwork},
	{"netlink_", ReasonNetwork},
	{"udp_", ReasonNetwork},
	{"skb_wait", ReasonNetwork},

	{"ep_poll", ReasonPoll},
	{"do_epoll", ReasonPoll},
	{"do_select", ReasonPoll},
	{"do_sys_poll", ReasonPoll},
	{"poll_schedule", ReasonPoll},

	{"nanosleep", ReasonSleep},
	{"do_sys_clock", ReasonSleep},
	{"msleep", ReasonSleep},
}

// Classify names the reason a stack represents.
//
// Frames are given innermost first, as the kernel returns them, and scanned
// in that order: the deepest call that means something is the one that
// decided to stop. Scanning from the other end would find the syscall entry
// point every time.
func Classify(frames []string) Reason {
	for _, frame := range frames {
		for _, pattern := range reasonPatterns {
			if strings.Contains(frame, pattern.needle) {
				return pattern.reason
			}
		}
	}
	return ReasonOther
}

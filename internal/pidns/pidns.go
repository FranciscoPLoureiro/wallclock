// Package pidns answers whether this process shares the pid namespace that
// BPF reports pids in.
//
// It matters more than it sounds, and it keeps mattering in new places, which
// is why it is a package rather than a helper next to the first thing that
// needed it. bpf_get_current_pid_tgid and the scheduler tracepoints always
// report pids as the *initial* namespace numbers them, no matter where the
// observer sits. Inside a container -- or a WSL distribution, which is the
// same arrangement with a friendlier name -- /proc numbers the same processes
// differently, and the two sets overlap. So a pid taken from ps and handed to
// an in-kernel filter matches nothing, or matches something else entirely,
// and the tool reports a quiet, plausible, wrong answer: an idle process.
package pidns

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// initialInode is PROC_PID_INIT_INO, the fixed inode the kernel gives the
// initial pid namespace. Namespaces created later get inodes allocated from
// the nsfs pool, so a process whose /proc/self/ns/pid is anything else is
// inside a namespace of its own.
const initialInode = 0xEFFFFFFC

// InInitial reports whether this process is in the initial pid namespace, and
// therefore whether the pids it reads from /proc mean the same thing as the
// pids the kernel reports.
func InInitial() (bool, error) {
	var st unix.Stat_t
	if err := unix.Stat("/proc/self/ns/pid", &st); err != nil {
		return false, fmt.Errorf("stat /proc/self/ns/pid: %w", err)
	}
	return st.Ino == initialInode, nil
}

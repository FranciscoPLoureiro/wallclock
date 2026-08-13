package syscount

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// initialPIDNamespaceInode is PROC_PID_INIT_INO, the fixed inode the kernel
// gives the initial pid namespace. Namespaces created later get inodes
// allocated from the nsfs pool, so a process whose /proc/self/ns/pid is
// anything else is inside a namespace of its own.
const initialPIDNamespaceInode = 0xEFFFFFFC

// InInitialPIDNamespace reports whether this process shares the pid namespace
// that BPF reports pids in.
//
// It matters more than it sounds. bpf_get_current_pid_tgid always returns the
// pid as the initial namespace numbers it, no matter where the observer sits.
// Inside a container -- or a WSL distribution, which is the same arrangement
// with a friendlier name -- /proc numbers the same processes differently, and
// the two sets overlap. So a pid taken from `ps` and handed to the in-kernel
// filter matches nothing, or matches something else entirely, and the tool
// reports a quiet, plausible, wrong answer: an idle process.
//
// The remedy is not to guess. Callers are told, and the numbers are labelled
// for what they are.
func InInitialPIDNamespace() (bool, error) {
	var st unix.Stat_t
	if err := unix.Stat("/proc/self/ns/pid", &st); err != nil {
		return false, fmt.Errorf("stat /proc/self/ns/pid: %w", err)
	}
	return st.Ino == initialPIDNamespaceInode, nil
}

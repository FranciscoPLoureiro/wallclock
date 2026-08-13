package syscount

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// CgroupIDOf resolves a cgroup v2 directory to the 64-bit id the kernel uses
// for it, which is what bpf_get_current_cgroup_id returns and therefore what
// the in-kernel filter compares against.
//
// The id is the inode number of the directory. That is not a coincidence to
// be relied on loosely: cgroupfs is backed by kernfs, the kernel derives the
// cgroup id from the kernfs node id, and for cgroup v2 that is the inode
// number userspace sees. It is the same identity by two names rather than two
// identities that happen to agree.
func CgroupIDOf(path string) (uint64, error) {
	var st unix.Stat_t
	if err := unix.Stat(path, &st); err != nil {
		return 0, fmt.Errorf("stat %s: %w", path, err)
	}

	// A path that is not on the unified hierarchy would produce an inode
	// number that is a valid uint64 and matches nothing, which is the worst
	// possible failure: a filter that silently excludes everything and looks
	// like an idle machine.
	var fs unix.Statfs_t
	if err := unix.Statfs(path, &fs); err != nil {
		return 0, fmt.Errorf("statfs %s: %w", path, err)
	}
	if fs.Type != unix.CGROUP2_SUPER_MAGIC {
		return 0, fmt.Errorf("%s is not on a cgroup v2 filesystem (magic 0x%x)", path, fs.Type)
	}
	return st.Ino, nil
}

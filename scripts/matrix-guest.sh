#!/bin/sh
# Runs inside a vimto guest, once per kernel. Driven by scripts/kernel-matrix.sh,
# which parses the MATRIX_ lines below and never reads anything else.
#
# Every interesting fact is printed on a line with a MATRIX_ prefix, and the
# last thing this script does is print MATRIX_DONE. The host requires that
# marker before it will call a row a result. That is not decoration: the
# previous attempt at this matrix recorded a green refusal for a kernel where
# the tool had never run, because the guest hung, the deadline killed QEMU,
# and the word the assertion matched appeared in the timeout message. A
# sentinel printed after the work cannot be produced by a hang.
set -u

echo "MATRIX_UNAME=$(uname -r)"

# ci-kernels images boot with /sys/fs/cgroup as an empty sysfs directory --
# they carry a kernel and its modules, not an init that mounts anything. Both
# of wallclock's cgroup requirements fail against that, for a reason that is
# about the image rather than about the kernel under test.
# A real distribution has already done this before anything of ours runs, and
# mounting a second cgroup2 over the top would shadow the hierarchy systemd is
# using rather than add anything.
if [ "$(stat -fc %T /sys/fs/cgroup 2>/dev/null)" = "cgroup2fs" ]; then
	echo "MATRIX_CGROUP_MOUNT=already"
elif mount -t cgroup2 none /sys/fs/cgroup 2>/dev/null; then
	echo "MATRIX_CGROUP_MOUNT=ok"
else
	echo "MATRIX_CGROUP_MOUNT=failed"
fi
# Delegating cpu to children is what makes cpu.max and cpu.stat exist, which
# is what the throttling tests measure against.
echo "+cpu" > /sys/fs/cgroup/cgroup.subtree_control 2>/dev/null || true
echo "MATRIX_CONTROLLERS=$(cat /sys/fs/cgroup/cgroup.controllers 2>/dev/null || echo none)"

# Recorded, because the offsets these programs read are taken from here at
# load time and a kernel that moves them is the difference between a correct
# answer and a confident wrong one. When a row starts failing, the first
# question is what moved, and "the kernel" is only a satisfying answer when the
# previous layout was written down.
for tp in sched/sched_switch sched/sched_wakeup raw_syscalls/sys_enter; do
	echo "MATRIX_LAYOUT=$tp $(sed -n 's/.*field:\([^;]*\);\s*offset:\([0-9]*\);\s*size:\([0-9]*\).*/\1@\2:\3/p' \
		"/sys/kernel/tracing/events/$tp/format" 2>/dev/null | tr '\n' ' ')"
done

echo "MATRIX_PREFLIGHT_BEGIN"
./bin/wallclock-static preflight
preflight=$?
echo "MATRIX_PREFLIGHT_END"
echo "MATRIX_PREFLIGHT_EXIT=$preflight"

# Tests only where the host claims to be supported. On a kernel below the
# floor the interesting assertion is the refusal itself, and running a suite
# that cannot load anything would only produce noise to read through.
if [ "$preflight" -eq 0 ]; then
	for pkg in bpfload syscount offcpu netlat; do
		echo "MATRIX_TEST_BEGIN=$pkg"
		(cd "internal/$pkg" && WALLCLOCK_REQUIRE_BPF=1 "../../build/tests/$pkg.test" -test.v -test.timeout 240s)
		echo "MATRIX_TEST_EXIT=$pkg:$?"
	done
fi

echo "MATRIX_DONE"

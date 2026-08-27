#!/bin/sh
# Run wallclock, offcputime and runqlat against the same subjects, and print
# all three outputs.
#
# The README claims these tools are not composable into what wallclock
# produces. A claim like that is worth nothing without evidence, so this
# script produces the evidence and anybody can re-run it.
#
# Two subjects, chosen so the comparison is fair rather than flattering:
#
#   wc-capped   a process that never stops asking for CPU, inside a cgroup
#               allowed 20% of one. It is never blocked on anything -- it is
#               ready and forbidden -- which is the case the existing tools
#               answer in a way that reads as something else entirely.
#   wc-napper   a process that falls asleep inside the observation window and
#               is still asleep when it ends. The most interesting kind of
#               wait there is, and the kind an off-CPU profiler cannot see.
#
# Both subjects are in a steady state for the whole run, which is what makes
# it fair to measure them one tool at a time -- and one tool at a time is
# forced, see the note on staggering below.
#
# Needs root. bpfcc-tools supplies offcputime-bpfcc and runqlat-bpfcc; on
# Debian and Ubuntu that is `apt-get install bpfcc-tools`.
set -e

WINDOW="${WINDOW:-10}"
CGROUP_ROOT=/sys/fs/cgroup
CGROUP="$CGROUP_ROOT/wallclock-compare-$$"
WORKDIR=$(mktemp -d)
BIN="${BIN:-./bin/wallclock}"

OFFCPUTIME="${OFFCPUTIME:-offcputime-bpfcc}"
RUNQLAT="${RUNQLAT:-runqlat-bpfcc}"

# How long each tool needs before it is actually watching. bcc compiles its
# programs from C at startup and takes seconds over it; wallclock's are
# compiled ahead of time and embedded in the binary, so it is attached almost
# immediately. The subject is made to fall asleep after this delay so that
# every tool gets the same chance to see it happen.
BCC_WARMUP="${BCC_WARMUP:-8}"
WALLCLOCK_WARMUP=1

cleanup() {
	if [ -n "$CAPPED_PID" ]; then kill "$CAPPED_PID" 2>/dev/null || true; fi
	if [ -n "$NAPPER_PID" ]; then kill "$NAPPER_PID" 2>/dev/null || true; fi
	# The cgroup will not go while anything is in it, and the spinner needs a
	# moment to actually die after the signal.
	sleep 0.3
	if [ -d "$CGROUP" ]; then rmdir "$CGROUP" 2>/dev/null || true; fi
	rm -rf "$WORKDIR"
}
trap cleanup EXIT INT TERM

require() {
	command -v "$1" >/dev/null 2>&1 || {
		echo "$1 is not on PATH." >&2
		echo "  Debian/Ubuntu: apt-get install bpfcc-tools" >&2
		exit 1
	}
}
require "$OFFCPUTIME"
require "$RUNQLAT"
[ -x "$BIN" ] || { echo "$BIN not built: run make build" >&2; exit 1; }
[ "$(id -u)" -eq 0 ] || { echo "needs root" >&2; exit 1; }

# comm comes from the executable name and the kernel does not read argv[0] for
# it, so the subjects are copies of real binaries under the names the reports
# will show. `exec -a` does not do this.
# Which shell that is varies by distribution -- Debian ships /bin/dash, Arch
# and Fedora do not -- and /bin/sh is the one POSIX guarantees. Same list, and
# the same reasoning, as internal/spin.
SPIN_SHELL=""
for candidate in /bin/dash /bin/sh /bin/bash /usr/bin/dash /usr/bin/sh /usr/bin/bash; do
	[ -x "$candidate" ] && { SPIN_SHELL="$candidate"; break; }
done
[ -n "$SPIN_SHELL" ] || { echo "no shell to copy as the spinner" >&2; exit 1; }
cp "$SPIN_SHELL" "$WORKDIR/wc-capped"
cp /bin/sleep "$WORKDIR/wc-napper"

# A cgroup allowed 20 ms of CPU in every 100 ms period.
grep -qw cpu "$CGROUP_ROOT/cgroup.subtree_control" || \
	echo '+cpu' > "$CGROUP_ROOT/cgroup.subtree_control"
mkdir -p "$CGROUP"
echo '20000 100000' > "$CGROUP/cpu.max"

# The spinner runs throughout. It is never off a CPU for any reason other than
# its quota, so it looks the same to whichever tool is watching.
"$WORKDIR/wc-capped" -c 'while :; do :; done' &
CAPPED_PID=$!
echo "$CAPPED_PID" > "$CGROUP/cgroup.procs"

# A fresh sleeper per tool, started once that tool is attached, so each one
# has the same opportunity to observe a thread falling asleep and staying
# asleep.
nap_during() {
	sleep "$1"
	"$WORKDIR/wc-napper" "$((WINDOW + 10))" &
	NAPPER_PID=$!
}

end_nap() {
	if [ -n "$NAPPER_PID" ]; then
		kill "$NAPPER_PID" 2>/dev/null || true
		# Reaped before the variable is cleared, so that the next tool does
		# not see this one exiting. Clearing it first passed an empty
		# argument to wait, which waits for everything -- including the
		# spinner, which never returns.
		wait "$NAPPER_PID" 2>/dev/null || true
		NAPPER_PID=
	fi
}

echo "subjects: wc-capped spinning under a 20% quota, wc-napper asleep"
echo "window:   ${WINDOW}s per tool, against the same two subjects"
echo
echo "One tool at a time, and that is forced rather than chosen. Starting two bcc"
echo "tools at once makes one of them die with \"Unable to find kernel headers\":"
echo "they both read /sys/kernel/kheaders.tar.xz to compile against, and on this"
echo "kernel that file does not take two readers at once -- the loser gets ENODEV"
echo "half way through the archive. Each works alone. Both subjects hold a steady"
echo "state throughout, so measuring them in turn is fair; it is worth noticing"
echo "that the tools cannot even be started together."
echo

"$BIN" profile -for "${WINDOW}s" -comm wc- -reasons -top 4 > "$WORKDIR/wallclock.txt" 2>&1 &
JOB=$!
nap_during "$WALLCLOCK_WARMUP"
wait "$JOB" || true
end_nap

"$OFFCPUTIME" "$WINDOW" > "$WORKDIR/offcputime.txt" 2>&1 &
JOB=$!
nap_during "$BCC_WARMUP"
wait "$JOB" || true
end_nap

"$RUNQLAT" "$WINDOW" 1 > "$WORKDIR/runqlat.txt" 2>&1 &
JOB=$!
nap_during "$BCC_WARMUP"
wait "$JOB" || true
end_nap

echo '=============================== wallclock ==============================='
cat "$WORKDIR/wallclock.txt"

echo
echo '=============================== offcputime =============================='
# offcputime prints one stack per distinct call path and they are long. The
# subjects are what this is about, so what is shown is the count against each
# of them and the deepest stack it produced.
capped_stacks=$(grep -c 'wc-capped' "$WORKDIR/offcputime.txt" || true)
napper_stacks=$(grep -c 'wc-napper' "$WORKDIR/offcputime.txt" || true)
capped_usec=$(grep -A1 'wc-capped' "$WORKDIR/offcputime.txt" \
	| grep -E '^ +[0-9]+$' | awk '{s+=$1} END {print s+0}')
echo "wc-capped: $capped_stacks separate stacks, ${capped_usec}us off-CPU in total"
echo "wc-napper: $napper_stacks stacks"
echo
echo "the largest single stack it reports for wc-capped:"
grep -B10 -E "^ +$(grep -A1 'wc-capped' "$WORKDIR/offcputime.txt" \
	| grep -E '^ +[0-9]+$' | sort -n | tail -1 | tr -d ' ')\$" \
	"$WORKDIR/offcputime.txt" | head -12

echo
echo '================================ runqlat ==============================='
cat "$WORKDIR/runqlat.txt"

#!/usr/bin/env bash
# Run wallclock's kernel-dependent tests against a range of kernels, under
# QEMU, on this host.
#
# The claim this exists to settle is the one on the first screen of the
# README: compiled once, loaded by any kernel new enough. Two kernels eleven
# minor versions apart, both above 6.6, do not settle it -- CO-RE relocations
# have nothing to do in a neighbourhood that never moved. What settles it is a
# span, and a kernel below the stated floor where the tool has to refuse.
#
# Needs: vimto (go install lmb.io/vimto@latest), qemu-system-x86_64, and a
# usable /dev/kvm. Without KVM, QEMU falls back to software emulation and the
# guests are slow enough to look like hangs -- which is exactly how the first
# attempt at this matrix failed, seven CI rounds in a row, on a runner where
# /dev/kvm exists but is not readable by the runner user.
set -uo pipefail

cd "$(dirname "$0")/.."
ROOT=$(pwd)
# shellcheck source=scripts/matrix-lib.sh
. scripts/matrix-lib.sh

IMAGE=${IMAGE:-ghcr.io/cilium/ci-kernels}
# 4.19 and 5.4 are below the 5.8 floor and must be refused. The rest span the
# floor to the tip.
KERNELS=${KERNELS:-"4.19 5.4 5.10 5.15 6.1 6.6 6.10 stable"}
# GOBIN is set on a developer machine that uses a version manager and unset
# almost everywhere else, where `go install` writes to GOPATH/bin instead. The
# first version of this looked only at GOBIN, so on a fresh checkout -- a CI
# runner, most obviously -- it resolved to the literal path "/vimto".
if [ -z "${VIMTO:-}" ]; then
	VIMTO=$(command -v vimto || true)
	for candidate in "$(go env GOBIN)/vimto" "$(go env GOPATH)/bin/vimto"; do
		[ -n "$VIMTO" ] && break
		[ -x "$candidate" ] && VIMTO="$candidate"
	done
fi
LOGDIR=${LOGDIR:-build/matrix}
# Generous, because the point of the deadline is to catch a hang, not to race
# a slow guest. A working guest finishes in well under a minute.
DEADLINE=${DEADLINE:-600}

[ -x "$VIMTO" ] || { echo "vimto not found: go install lmb.io/vimto@latest" >&2; exit 1; }
[ -r /dev/kvm ] || echo "warning: /dev/kvm is not readable; guests will be emulated and may look like hangs" >&2

mkdir -p "$LOGDIR"

echo "building the static artefacts the guests run"
CGO_ENABLED=0 go build -buildvcs=false -o bin/wallclock-static ./cmd/wallclock || exit 1
mkdir -p build/tests
for pkg in $MATRIX_PACKAGES; do
	CGO_ENABLED=0 go test -buildvcs=false -c -o "build/tests/$pkg.test" "./internal/$pkg" || exit 1
done

rows=()
failures=0

for kernel in $KERNELS; do
	log="$LOGDIR/$kernel.log"
	printf '%-8s ' "$kernel"

	timeout "$DEADLINE" "$VIMTO" -sudo -smp 4 -memory 2G \
		-kernel "$IMAGE:$kernel" \
		exec /bin/sh "$ROOT/scripts/matrix-guest.sh" >"$log" 2>&1
	vimto_status=$?

	# Turned into newlines, not deleted. vimto draws a download progress
	# spinner that overwrites itself with \r, so deleting them splices every
	# frame of that animation onto the front of the guest's first line of
	# output -- and every ^MATRIX_ anchor below stops matching. It only shows
	# on a kernel whose image is not cached yet, which is why it survived the
	# first run: the one kernel already in the cache parsed perfectly.
	tr '\r' '\n' <"$log" >"$log.clean" && mv "$log.clean" "$log"

	row=$(matrix_evaluate "$log" "$vimto_status" "$DEADLINE")
	held=$?
	rows+=("$row")
	[ "$held" -eq 0 ] || failures=$((failures + 1))
	IFS='|' read -r _ _ result notes <<<"$row"
	echo "$result — $notes"
done

echo
matrix_print_table "${rows[@]}"
echo
echo "logs in $LOGDIR/"

[ "$failures" -eq 0 ] || { echo "$failures of ${#rows[@]} rows did not hold"; exit 1; }
echo "every row held"

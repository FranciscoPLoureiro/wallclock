#!/usr/bin/env bash
# Phase 6, run again on bare metal.
#
# The question is the one the ticket office's README left open: its p99 went
# from 153 ms to 291 ms once Prometheus and Grafana shared the machine, and it
# could not say where the difference went. The first answer to it was measured
# under WSL2, and it came with a confounder nobody could resolve there -- the
# gap between the two conditions was 17% of throughput under a light profiling
# protocol and 2% under a heavy one. Two numbers that describe the same pair of
# conditions and disagree.
#
# A hypervisor with a busy Windows host underneath is a good way to produce
# exactly that: the profiler's own cost competes for CPU that the host is also
# handing to something else, and the competition is not the same from one run
# to the next. This host has no hypervisor. If the disagreement survives here
# it is a property of the system; if it does not, it was a property of the
# measurement.
#
# Every condition is run under every protocol, which is what the first attempt
# did not do -- "with, one 20s profile" was never measured, so the comparison
# had a hole in exactly the cell that would have settled it.
set -uo pipefail

cd "$(dirname "$0")/../.."
ROOT=$(pwd)

TICKET=${TICKET:-$ROOT/../HighConcurrencyTicketOffice}
OUT=${OUT:-build/phase6}
BIN=${BIN:-$ROOT/bin/wallclock}
# Loading BPF needs root; the load generator does not, and neither does the
# rest of this. The elevation goes on the tool rather than on the script.
SUDO=${SUDO:-$([ "$(id -u)" -eq 0 ] || echo "sudo -n")}
VUS=${VUS:-100}
# Three runs of every cell, not one.
#
# The first attempt at this measured each cell once and then reported that two
# protocols disagreed about the size of the effect -- 17% of throughput against
# 2%. With one sample per cell that is not a disagreement, because neither
# number has a spread to disagree outside of. This project's own overhead
# section says as much about this very target: its documented p99 varies from
# 153 ms to 333 ms and the effect being looked for is a few per cent, so the
# signal sits under the noise unless the noise is measured too. Three runs is
# the cheapest thing that produces a spread at all.
REPEATS=${REPEATS:-3}
K6_IMAGE=${K6_IMAGE:-grafana/k6:latest}
# sudo strips the environment, so a variable compose has to substitute cannot
# simply be exported: `RATE_LIMIT_IP=... sudo docker compose` hands sudo a
# variable it discards, compose falls back to the file's default of a thousand
# a second, and the run spends its whole plateau being rate limited while the
# summary looks ordinary. Handed to env on the far side of sudo instead.
RATE_LIMIT_IP=${RATE_LIMIT_IP:-100000000}
DOCKER=${DOCKER:-sudo -n env RATE_LIMIT_IP=$RATE_LIMIT_IP docker}
# The observability pair, which is the whole independent variable.
OBSERVABILITY="prometheus grafana"
# Overridable so one cell can be run on its own while the harness is being
# checked, without editing the sweep.
CONDITIONS=${CONDITIONS:-"without with"}
PROTOCOLS=${PROTOCOLS:-"destinations one-profile seven-profiles"}

[ -d "$TICKET" ] || { echo "ticket office not found at $TICKET" >&2; exit 1; }
[ -x "$BIN" ] || { echo "$BIN not built: run make build" >&2; exit 1; }

mkdir -p "$OUT"

# The per-IP limit defaults to a thousand a second, which is right for a
# service on the internet and wrong here: every virtual user comes from the one
# generator container, so they share a bucket and the limiter refuses almost
# everything. Raised where DOCKER is defined, above.
compose() { (cd "$TICKET" && $DOCKER compose "$@"); }

# Derived, not assumed. Compose names the network after the project, and the
# project is whatever the compose file says it is rather than the directory it
# sits in -- here it declares `ticket-office` while the checkout is called
# something else entirely, so guessing from the path gets it wrong.
NETWORK=${NETWORK:-$(compose config --format json 2>/dev/null |
	python3 -c 'import sys,json;print(json.load(sys.stdin)["name"])' 2>/dev/null)_default}

# The api container's cgroup, found rather than assumed. Docker writes it under
# system.slice with the systemd driver and under /sys/fs/cgroup/docker with the
# cgroupfs one, and the first measurement of this experiment was taken on a host
# that used the second.
api_cgroup() {
	local id
	id=$(compose ps -q api 2>/dev/null | head -1)
	[ -n "$id" ] || return 1
	local full
	full=$($DOCKER inspect -f '{{.Id}}' "$id" 2>/dev/null) || return 1
	local path
	path=$(sudo find /sys/fs/cgroup -maxdepth 4 -type d -name "*$full*" 2>/dev/null | head -1)
	[ -n "$path" ] || return 1
	echo "$path"
}

# k6's own summary, read from its text output rather than --summary-export:
# the flag has been deprecated and undeprecated across releases and the table
# has not moved in years.
# k6's own summary, read from its text output rather than --summary-export:
# the flag has been deprecated and undeprecated across releases and the table
# has not moved in years.
k6_rate() { # "http_reqs...: 9210416 43859.08995/s" -> 43859.08995
	grep -E "^\s+$2[. ]*:" "$1" | head -1 | grep -oE '[0-9.]+/s' | head -1 | tr -d '/s'
}
k6_stat() { # "http_req_duration...: avg=2.17ms ... p(99)=..." -> the named one
	grep -E "^\s+$2[. ]*:" "$1" | head -1 |
		grep -oE "$3=[0-9.]+[a-zµ]*" | head -1 | cut -d= -f2
}

run_one() {
	local condition=$1 protocol=$2 repeat=$3
	local tag="$condition-$protocol-$repeat"
	local dir="$OUT/$tag"
	rm -rf "$dir"; mkdir -p "$dir"

	echo "=== $tag ==="

	# Everything healthy first, and only then take the observability pair away
	# again for the condition that does not want it.
	#
	# The other order does not work and does not look broken: `compose stop
	# prometheus grafana` followed by `compose up -d --wait` starts them
	# straight back up, because --wait brings up every service in the file.
	# Both conditions would then have run with the full stack, the comparison
	# would have been between a stack and itself, and the answer -- no
	# difference -- would have been perfectly reproducible and meaningless.
	compose up -d --wait >/dev/null 2>&1
	if [ "$condition" = "with" ]; then
		compose start $OBSERVABILITY >/dev/null 2>&1
	else
		compose stop $OBSERVABILITY >/dev/null 2>&1
	fi

	# Checked rather than assumed, because the independent variable of the
	# whole experiment is exactly this and nothing downstream would notice.
	local running
	running=$(compose ps --services --filter status=running 2>/dev/null | grep -cE '^(prometheus|grafana)$')
	if [ "$condition" = "with" ] && [ "$running" -ne 2 ]; then
		echo "  observability should be up and $running of 2 are"; return 1
	fi
	if [ "$condition" = "without" ] && [ "$running" -ne 0 ]; then
		echo "  observability should be down and $running of 2 are up"; return 1
	fi

	# The campaign is a hundred tickets because a hundred is the number the
	# oversell invariant is tested against, and that is the right size for the
	# test it was written for. It is the wrong size for this experiment: at a
	# hundred virtual users the stock is gone inside a second and the remaining
	# three minutes measure the refusal path, which answers out of Redis and
	# never reaches PostgreSQL or RabbitMQ at all. Two thirds of the
	# destination view -- half of what is being compared here -- would be
	# empty, and the API would be doing none of the work the question is about.
	#
	# Raised before the reset rather than after it, because the reset is what
	# rebuilds Redis from PostgreSQL, and a stock changed afterwards would be
	# invisible to the path that actually serves requests.
	if ! compose exec -T postgres psql -q -U tickets -d tickets \
		-c "UPDATE tickets SET total = 50000000;" >"$dir/setup.log" 2>&1; then
		echo "  could not raise the stock; see $dir/setup.log"
		return 1
	fi
	# DOCKER is passed through because the ticket office's Makefile calls
	# `docker` unelevated and this user is not in the docker group. The first
	# version of this ran it without, sent its output to /dev/null and ignored
	# the status -- so every reset failed, every run started sold out, and the
	# generator spent three and a half minutes measuring refusals while the
	# summary looked entirely normal. Checked now, and loudly.
	# The redirection is outside the subshell on purpose: inside it, after the
	# cd, a relative log path resolves against the ticket office's directory
	# instead of this one.
	if ! (cd "$TICKET" && make reset DOCKER="$DOCKER") >>"$dir/setup.log" 2>&1; then
		echo "  reset failed; see $dir/setup.log"
		return 1
	fi
	sleep 5

	local cgroup
	cgroup=$(api_cgroup) || { echo "  could not find the api cgroup"; return 1; }
	echo "  api cgroup: $cgroup"

	# The generator runs in a container on the compose network, so it reaches
	# the api by service name and its own latency is not measured across a
	# published port.
	$DOCKER run --rm --network "$NETWORK" \
		-v "$ROOT/scripts/phase6":/loadtest \
		-e BASE_URL=http://api:8080 -e VUS="$VUS" -e PLATEAU=200s \
		"$K6_IMAGE" run /loadtest/load.js >"$dir/k6.log" 2>&1 &
	local k6pid=$!

	# Let the ramp finish before any window opens. A window that straddles the
	# ramp measures the ramp.
	sleep 12

	case "$protocol" in
	destinations)
		$SUDO "$BIN" destinations -for 25s -cgroup "$cgroup" >"$dir/destinations.txt" 2>&1
		;;
	one-profile)
		$SUDO "$BIN" profile -for 20s -cgroup "$cgroup" -top 12 >"$dir/profile-1.txt" 2>&1
		$SUDO "$BIN" destinations -for 25s -cgroup "$cgroup" >"$dir/destinations.txt" 2>&1
		;;
	seven-profiles)
		for i in $(seq 7); do
			$SUDO "$BIN" profile -for 25s -cgroup "$cgroup" -top 12 >"$dir/profile-$i.txt" 2>&1
		done
		$SUDO "$BIN" destinations -for 25s -cgroup "$cgroup" >"$dir/destinations.txt" 2>&1
		;;
	esac

	wait "$k6pid"

	# A cell in which nothing was ever bought is not a measurement of this
	# system, whatever its latency numbers look like. Two of the three
	# destinations are never reached on a refusal, so the comparison would be
	# between two runs of the rate limiter.
	local sold
	sold=$(grep -oE 'tickets_sold[. ]*:[^0-9]*[0-9]+' "$dir/k6.log" | grep -oE '[0-9]+$' | head -1)
	if [ -z "${sold:-}" ] || [ "$sold" -eq 0 ]; then
		echo "  WARNING: no tickets were sold in this run -- it measured refusals"
	fi

	local reqs mean p99
	reqs=$(k6_rate "$dir/k6.log" http_reqs)
	mean=$(k6_stat "$dir/k6.log" http_req_duration avg)
	p99=$(k6_stat "$dir/k6.log" http_req_duration 'p\(99\)')
	echo "$condition|$protocol|$repeat|${reqs:-?}|${mean:-?}|${p99:-?}" >>"$OUT/summary.txt"
	echo "  $reqs req/s, mean $mean, p99 $p99"
}

: >"$OUT/summary.txt"
# Repeat is the outer loop, so the three samples of a cell are separated by
# every other cell rather than taken back to back. Three consecutive runs of
# one configuration share whatever the machine was doing for those twelve
# minutes; three spread across the sweep do not, and a drift over the hour --
# thermal, or a background job -- lands on every cell instead of on one.
for repeat in $(seq "$REPEATS"); do
	for condition in $CONDITIONS; do
		for protocol in $PROTOCOLS; do
			run_one "$condition" "$protocol" "$repeat"
		done
	done
done

echo
printf '%-10s %-16s %-4s %-12s %-10s %s\n' condition protocol run req/s mean p99
printf '%-10s %-16s %-4s %-12s %-10s %s\n' ---------- ---------------- ---- ------------ ---------- ---
while IFS='|' read -r c p n r m q; do
	printf '%-10s %-16s %-4s %-12s %-10s %s\n' "$c" "$p" "$n" "$r" "$m" "$q"
done <"$OUT/summary.txt"
echo
echo "everything in $OUT/"

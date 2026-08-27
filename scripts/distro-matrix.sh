#!/usr/bin/env bash
# Run wallclock's kernel-dependent tests against the kernels distributions
# actually ship, in QEMU, on this host.
#
# This is the companion to scripts/kernel-matrix.sh and it answers a different
# question. The ci-kernels images that one boots are built for BPF CI: minimal
# configs, no CONFIG_CFS_BANDWIDTH, so they can exercise loading and
# attachment but cannot throttle a cgroup and therefore cannot exercise the
# one category this tool exists to separate. These images can. They are also
# the kernels a reader of the README would run this on.
#
# The row worth having here is Rocky 9. Its kernel calls itself 5.14 and is
# nothing of the sort -- Red Hat backports years of subsequent work into a
# frozen version number -- so a struct layout matching upstream 5.14 is the
# one thing it will not have. That is the case CO-RE exists for, and no
# mainline kernel tests it.
set -uo pipefail

cd "$(dirname "$0")/.."
ROOT=$(pwd)
# shellcheck source=scripts/matrix-lib.sh
. scripts/matrix-lib.sh

IMAGES=${IMAGES:-build/images}
WORK=${WORK:-build/vm}
LOGDIR=${LOGDIR:-build/distro-matrix}
# Cloud-init on a first boot does a lot: growing the filesystem, generating
# host keys, creating the user. Two minutes is unremarkable and five is not a
# hang.
BOOT_DEADLINE=${BOOT_DEADLINE:-420}
RUN_DEADLINE=${RUN_DEADLINE:-900}
MEMORY=${MEMORY:-2048}
CPUS=${CPUS:-4}

# name|url|what the row is for
DISTROS=${DISTROS:-"
ubuntu-20.04|https://cloud-images.ubuntu.com/focal/current/focal-server-cloudimg-amd64.img|below the floor on a kernel people really run
ubuntu-22.04|https://cloud-images.ubuntu.com/jammy/current/jammy-server-cloudimg-amd64.img|the oldest LTS above the floor
debian-12|https://cloud.debian.org/images/cloud/bookworm/latest/debian-12-genericcloud-amd64.qcow2|a stable distribution in the middle
rocky-9|https://dl.rockylinux.org/pub/rocky/9/images/x86_64/Rocky-9-GenericCloud-Base.latest.x86_64.qcow2|a vendor kernel whose version number is a fiction
ubuntu-24.04|https://cloud-images.ubuntu.com/noble/current/noble-server-cloudimg-amd64.img|the current LTS
"}

command -v qemu-system-x86_64 >/dev/null || { echo "qemu-system-x86_64 not installed" >&2; exit 1; }
command -v xorrisofs >/dev/null || { echo "xorrisofs not installed (pacman -S libisoburn)" >&2; exit 1; }
[ -r /dev/kvm ] || { echo "/dev/kvm is not readable; these guests would be emulated and unusably slow" >&2; exit 1; }

mkdir -p "$IMAGES" "$WORK" "$LOGDIR"

KEY="$WORK/id_ed25519"
if [ ! -f "$KEY" ]; then
	ssh-keygen -t ed25519 -N '' -C wallclock-matrix -f "$KEY" >/dev/null
fi
SSH_OPTS=(-o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null
	-o LogLevel=ERROR -o ConnectTimeout=5 -i "$KEY")

echo "building the static artefacts the guests run"
CGO_ENABLED=0 go build -buildvcs=false -o bin/wallclock-static ./cmd/wallclock || exit 1
mkdir -p build/tests
for pkg in $MATRIX_PACKAGES; do
	CGO_ENABLED=0 go test -buildvcs=false -c -o "build/tests/$pkg.test" "./internal/$pkg" || exit 1
done

# One seed for every guest. Nothing in it is per-distro, and a single image
# means one thing to be wrong about rather than five.
SEED="$WORK/seed.iso"
if [ ! -f "$SEED" ]; then
	mkdir -p "$WORK/seed"
	cat >"$WORK/seed/meta-data" <<-EOF
		instance-id: wallclock-matrix
		local-hostname: wallclock-matrix
	EOF
	cat >"$WORK/seed/user-data" <<-EOF
		#cloud-config
		users:
		  - name: wallclock
		    sudo: ALL=(ALL) NOPASSWD:ALL
		    shell: /bin/bash
		    lock_passwd: true
		    ssh_authorized_keys:
		      - $(cat "$KEY.pub")
		ssh_pwauth: false
	EOF
	xorrisofs -quiet -output "$SEED" -volid cidata -joliet -rock \
		"$WORK/seed/user-data" "$WORK/seed/meta-data" || exit 1
fi

rows=()
failures=0
port=2222

# Read into an array first. Iterating $DISTROS directly would word-split on
# the spaces inside each entry's description, and looping over a here-string
# would have ssh and tar eating the list off stdin as they go.
mapfile -t entries <<<"$DISTROS"

for entry in "${entries[@]}"; do
	IFS='|' read -r name url purpose <<<"$entry"
	[ -n "${name:-}" ] || continue
	[ -n "${url:-}" ] || continue
	log="$LOGDIR/$name.log"
	console="$LOGDIR/$name.console.log"
	printf '%-14s ' "$name"

	base="$IMAGES/$name.qcow2"
	if [ ! -f "$base" ]; then
		printf 'downloading... '
		if ! curl -fsSL --retry 3 -o "$base.part" "$url"; then
			rows+=("$name|-|no result|could not download $url")
			failures=$((failures + 1))
			echo "NO RESULT (download)"
			rm -f "$base.part"
			continue
		fi
		mv "$base.part" "$base"
	fi

	# An overlay per run, discarded after it. The base image stays pristine,
	# so a second run of this script measures the same starting state as the
	# first -- and a guest that corrupts its disk costs one boot rather than
	# one download.
	overlay="$WORK/$name.overlay.qcow2"
	rm -f "$overlay"
	qemu-img create -q -f qcow2 -F qcow2 -b "$ROOT/$base" "$overlay" 20G || {
		rows+=("$name|-|no result|could not create the overlay disk")
		failures=$((failures + 1)); echo "NO RESULT (overlay)"; continue
	}

	pidfile="$WORK/$name.pid"
	qemu-system-x86_64 \
		-enable-kvm -cpu host -smp "$CPUS" -m "$MEMORY" \
		-drive "file=$overlay,if=virtio,format=qcow2" \
		-drive "file=$SEED,media=cdrom" \
		-nic "user,model=virtio-net-pci,hostfwd=tcp:127.0.0.1:$port-:22" \
		-display none -serial "file:$console" \
		-daemonize -pidfile "$pidfile" 2>>"$console" || {
		rows+=("$name|-|no result|qemu would not start")
		failures=$((failures + 1)); echo "NO RESULT (qemu)"; continue
	}

	shutdown_vm() {
		ssh "${SSH_OPTS[@]}" -p "$port" wallclock@127.0.0.1 "sudo poweroff" >/dev/null 2>&1
		for _ in $(seq 30); do
			[ -f "$pidfile" ] || break
			kill -0 "$(cat "$pidfile" 2>/dev/null)" 2>/dev/null || break
			sleep 1
		done
		[ -f "$pidfile" ] && kill -9 "$(cat "$pidfile")" 2>/dev/null
		rm -f "$pidfile" "$overlay"
	}

	printf 'booting... '
	booted=0
	deadline=$((SECONDS + BOOT_DEADLINE))
	while [ "$SECONDS" -lt "$deadline" ]; do
		if ssh "${SSH_OPTS[@]}" -p "$port" wallclock@127.0.0.1 true 2>/dev/null; then
			booted=1
			break
		fi
		sleep 5
	done
	if [ "$booted" -ne 1 ]; then
		rows+=("$name|-|no result|no ssh within ${BOOT_DEADLINE}s (console in $console)")
		failures=$((failures + 1))
		echo "NO RESULT (boot)"
		shutdown_vm
		port=$((port + 1))
		continue
	fi

	# tar over ssh rather than rsync or scp -r: rsync is not in every cloud
	# image and tar is in all of them.
	printf 'copying... '
	tar -cf - --exclude=.git --exclude=build/images --exclude=build/vm \
		--exclude=build/matrix --exclude=build/distro-matrix \
		--exclude='build/*.log' . |
		ssh "${SSH_OPTS[@]}" -p "$port" wallclock@127.0.0.1 \
			"rm -rf wallclock && mkdir -p wallclock && tar -xf - -C wallclock" || {
		rows+=("$name|-|no result|could not copy the tree into the guest")
		failures=$((failures + 1)); echo "NO RESULT (copy)"; shutdown_vm; port=$((port + 1)); continue
	}

	printf 'running... '
	timeout "$RUN_DEADLINE" ssh "${SSH_OPTS[@]}" -p "$port" wallclock@127.0.0.1 \
		"cd wallclock && sudo sh scripts/matrix-guest.sh" >"$log" 2>&1
	status=$?
	tr '\r' '\n' <"$log" >"$log.clean" && mv "$log.clean" "$log"

	row=$(matrix_evaluate "$log" "$status" "$RUN_DEADLINE")
	held=$?
	rows+=("$row")
	[ "$held" -eq 0 ] || failures=$((failures + 1))
	IFS='|' read -r _ _ result notes <<<"$row"
	echo "$result — $notes"

	shutdown_vm
	port=$((port + 1))
done

echo
matrix_print_table "${rows[@]}"
echo
echo "logs in $LOGDIR/"

[ "$failures" -eq 0 ] || { echo "$failures of ${#rows[@]} rows did not hold"; exit 1; }
echo "every row held"

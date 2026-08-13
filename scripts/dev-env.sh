#!/usr/bin/env bash
# Bring a Debian or Ubuntu host to the state this project builds in.
#
# Idempotent by design: it is run again whenever a version below moves, and
# rerunning it on an already-provisioned host must be a no-op that still
# prints what it found. Every version is pinned here rather than left to the
# distribution, because the same versions are what CI installs, and a
# toolchain that differs between the two turns "works on my machine" into a
# debugging session instead of a build failure.
set -euo pipefail

GO_VERSION="${GO_VERSION:-1.26.5}"
GO_SHA256="${GO_SHA256:-5c2c3b16caefa1d968a94c1daca04a7ca301a496d9b086e17ad77bb81393f053}"
GOLANGCI_VERSION="${GOLANGCI_VERSION:-v2.12.2}"

# clang and llvm-strip compile the BPF objects; libbpf-dev supplies the
# bpf_helpers.h they include; bpftool reads kernel BTF; libelf and zlib are
# what libbpf itself links against. iproute2 is not needed to build, but the
# ground-truth validation later injects delay with `tc netem`, and finding it
# missing at that point costs a rebuild of the environment.
APT_PACKAGES=(
	clang llvm libbpf-dev
	git make gcc libelf-dev zlib1g-dev pkg-config
	curl ca-certificates iproute2
)

# bpftool is a real package on Debian and a virtual one on Ubuntu, where the
# binary ships inside linux-tools-common. Naming the wrong one does not skip a
# package, it aborts the entire install with "has no installation candidate",
# so the name is resolved against the archive rather than assumed.
bpftool_package() {
	local candidate
	candidate="$(apt-cache policy bpftool 2>/dev/null | awk '/Candidate:/ {print $2}')"
	if [ -n "$candidate" ] && [ "$candidate" != "(none)" ]; then
		echo bpftool
	else
		echo linux-tools-common
	fi
}

say() { printf '\033[1m==>\033[0m %s\n' "$*"; }

need_root() {
	if [ "$(id -u)" -ne 0 ]; then
		echo "This script installs packages and must run as root." >&2
		echo "On WSL:  wsl -d Debian -u root -- bash \$PWD/scripts/dev-env.sh" >&2
		echo "Elsewhere: sudo bash scripts/dev-env.sh" >&2
		exit 1
	fi
}

install_packages() {
	say "apt packages"
	DEBIAN_FRONTEND=noninteractive apt-get update -qq
	DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
		"${APT_PACKAGES[@]}" "$(bpftool_package)"
}

install_go() {
	local current=""
	if [ -x /usr/local/go/bin/go ]; then
		current="$(/usr/local/go/bin/go version | awk '{print $3}')"
	fi
	if [ "$current" = "go${GO_VERSION}" ]; then
		say "Go ${GO_VERSION} already installed"
		return
	fi

	say "Go ${GO_VERSION} (found: ${current:-none})"
	local tarball="/tmp/go${GO_VERSION}.linux-amd64.tar.gz"
	curl -fsSL -o "$tarball" "https://dl.google.com/go/go${GO_VERSION}.linux-amd64.tar.gz"
	# The checksum is the reason this installs from upstream rather than from
	# the distribution: it pins one exact toolchain, and a tarball that has
	# been swapped underneath us fails here instead of building silently.
	echo "${GO_SHA256}  ${tarball}" | sha256sum -c -
	rm -rf /usr/local/go
	tar -C /usr/local -xzf "$tarball"
	rm -f "$tarball"

	# /etc/profile.d rather than the invoking user's shell profile: this
	# script runs as root, and writing to root's profile would leave the
	# toolchain invisible to the user who actually builds.
	cat > /etc/profile.d/go.sh <<-'PROFILE'
	export PATH="/usr/local/go/bin:${PATH}:${HOME}/go/bin"
	PROFILE
	chmod 0644 /etc/profile.d/go.sh
}

install_golangci_lint() {
	local target_user="${SUDO_USER:-${WALLCLOCK_USER:-}}"
	if [ -z "$target_user" ]; then
		# WSL's `-u root` leaves no trace of who asked, so fall back to the
		# first ordinary account rather than installing into /root/go/bin,
		# where the developer's shell will never look for it.
		target_user="$(awk -F: '$3 >= 1000 && $3 < 65534 {print $1; exit}' /etc/passwd)"
	fi
	local home
	home="$(getent passwd "$target_user" | cut -d: -f6)"

	if [ -x "${home}/go/bin/golangci-lint" ] &&
		"${home}/go/bin/golangci-lint" version 2>/dev/null | grep -q "${GOLANGCI_VERSION#v}"; then
		say "golangci-lint ${GOLANGCI_VERSION} already installed for ${target_user}"
		return
	fi

	say "golangci-lint ${GOLANGCI_VERSION} for ${target_user}"
	sudo -u "$target_user" env PATH="/usr/local/go/bin:${PATH}" HOME="$home" \
		/usr/local/go/bin/go install \
		"github.com/golangci/golangci-lint/v2/cmd/golangci-lint@${GOLANGCI_VERSION}"
}

report() {
	say "installed"
	/usr/local/go/bin/go version
	clang --version | head -1
	# On Ubuntu this is a wrapper that dispatches to a linux-tools package
	# built for the running kernel, which does not exist for every cloud
	# kernel. bpftool is used for inspection and to generate vmlinux.h, never
	# in the build, so a missing one is worth printing and not worth failing
	# over -- hence 2>&1 rather than a check.
	printf 'bpftool %s\n' "$(bpftool version 2>&1 | head -1)"
	printf 'kernel  %s\n' "$(uname -r)"
	printf 'BTF     %s\n' \
		"$([ -r /sys/kernel/btf/vmlinux ] && echo present || echo MISSING)"
	printf 'cgroup  %s\n' "$(stat -fc %T /sys/fs/cgroup)"
	echo
	echo "Open a new shell (or 'source /etc/profile.d/go.sh'), then: make preflight"
}

need_root
install_packages
install_go
install_golangci_lint
report

# Local workflows for wallclock.
#
# Run `make` with no target for the list.

.DEFAULT_GOAL := help

CLANG      ?= clang
LLVM_STRIP ?= llvm-strip
GO         ?= go

BPF_SRC := $(wildcard bpf/*.bpf.c)
BPF_OBJ := $(patsubst bpf/%.bpf.c,build/%.bpf.o,$(BPF_SRC))
BIN     := bin/wallclock

# Fixtures: programs written to be refused, compiled next to the test that
# asserts the refusal. They build with the same flags as the real ones, which
# is the point -- a fixture compiled differently would prove the difference
# rather than the rejection.
FIXTURE_SRC := $(wildcard internal/*/testdata/*.bpf.c)
FIXTURE_OBJ := $(FIXTURE_SRC:.bpf.c=.bpf.o)

# Test packages that need a real kernel. They are run from their own
# directories by `make smoke`, so relative paths inside them mean the same
# thing as they do under `go test`.
KERNEL_TEST_DIRS := internal/bpfload internal/syscount internal/offcpu internal/netlat

# -target bpf makes clang emit BPF rather than host instructions.
#
# The include path is not decoration: linux/bpf.h pulls in linux/types.h,
# which includes <asm/types.h>, and Debian keeps the architecture headers
# under a multiarch directory that a cross-compilation target does not search
# by default. Without it the build fails with "asm/types.h file not found",
# which reads like a missing package and is not one.
#
# -g emits BTF, which is what CO-RE relocations are written against, so it is
# required rather than a debugging convenience. llvm-strip -g afterwards drops
# DWARF and keeps .BTF: same relocations, a third of the size.
#
# -Werror because a warning in a BPF program is usually the verifier's
# rejection arriving early, and early is cheaper.
#
# The include path is added only where it exists. Debian and Ubuntu keep the
# architecture headers under a multiarch directory that a cross-compilation
# target does not search by default, and without it the build fails with
# "asm/types.h file not found" -- which reads like a missing package and is
# not one. No other distribution has that directory: Arch, Fedora and Red Hat
# put those headers straight in /usr/include, which is already searched. The
# flag used to be passed unconditionally, and the comment above it described
# as necessary something that is necessary on one family and inert on the
# rest.
MULTIARCH := /usr/include/$(shell uname -m)-linux-gnu
BPF_CFLAGS := -target bpf -D__TARGET_ARCH_x86 \
	$(if $(wildcard $(MULTIARCH)),-I$(MULTIARCH)) \
	-O2 -g -Wall -Werror

# Loading a program needs root. When make is already running as root -- which
# is how this runs under `wsl -u root` and in CI -- sudo is not only
# unnecessary but often absent from the image.
SUDO ?= $(shell [ "$$(id -u)" -eq 0 ] || echo sudo)

# golangci-lint installs into GOPATH/bin, which is on PATH in an interactive
# shell and frequently not under sudo or in a fresh CI step.
GOLANGCI ?= $(shell command -v golangci-lint 2>/dev/null || echo "$$(go env GOPATH)/bin/golangci-lint")

# The working tree belongs to the developer and the load steps run as root.
# git refuses to report status for a repository owned by another user --
# "dubious ownership", the defence against a repo dropped somewhere writable
# carrying hooks -- and Go promotes that refusal to a build failure. Nothing
# reads the stamped VCS metadata yet, so it goes off rather than every root
# invocation needing a safe.directory entry.
GO_BUILDFLAGS ?= -buildvcs=false

.PHONY: help
help: ## Show this list
	@grep -hE '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) \
		| awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[1m%-14s\033[0m %s\n", $$1, $$2}'

.PHONY: dev-env
dev-env: ## Install the pinned toolchain on this host (needs root)
	$(SUDO) bash scripts/dev-env.sh

build/%.bpf.o: bpf/%.bpf.c
	@mkdir -p build
	$(CLANG) $(BPF_CFLAGS) -c $< -o $@
	$(LLVM_STRIP) -g $@

%.bpf.o: %.bpf.c
	$(CLANG) $(BPF_CFLAGS) -c $< -o $@
	$(LLVM_STRIP) -g $@

.PHONY: bpf
bpf: $(BPF_OBJ) $(FIXTURE_OBJ) ## Compile the BPF programs

.PHONY: generate
generate: ## Regenerate the bpf2go bindings and their embedded objects
	# Separate from `bpf` because the output is committed. The generated Go
	# and the object it embeds are what make `go build` work on a fresh clone
	# without clang, and CI checks they are current rather than trusting that
	# whoever changed the C remembered to run this.
	$(GO) generate ./...

.PHONY: build
build: ## Build the wallclock binary
	$(GO) build $(GO_BUILDFLAGS) -o $(BIN) ./cmd/wallclock

.PHONY: fmt
fmt: ## Format the Go sources
	$(GO) fmt ./...

.PHONY: vet
vet: ## Run go vet
	$(GO) vet ./...

.PHONY: lint
lint: ## Run golangci-lint
	$(GOLANGCI) run

.PHONY: test
test: bpf ## Run the tests
	# -race is cheap here and will not be later: phase 2 reads a ring buffer
	# from one goroutine while aggregating in another. -shuffle=on catches
	# tests that only pass in the order they happen to be written in.
	#
	# The tests that load programs into the kernel skip for an unprivileged
	# user; `make smoke` is the target that refuses to skip them.
	$(GO) test -race -shuffle=on ./...

.PHONY: verify
verify: bpf vet lint test ## Everything CI runs, in the same order

.PHONY: preflight
preflight: build ## Check this host against the requirements (needs root for the load check)
	$(SUDO) $(BIN) preflight

.PHONY: smoke
smoke: bpf build ## Run every test that needs a real kernel (needs root)
	# Compiled first and elevated second, rather than running `sudo go test`.
	# sudo resets PATH to secure_path, which has no /usr/local/go/bin, and it
	# hands the build a different HOME and therefore a cold module cache. A
	# test binary needs neither.
	#
	# Each binary then runs from its own package directory, because `go test`
	# does the same and the relative paths inside the tests are written for
	# that. Running them from the repository root instead resolves testdata
	# and ../../build somewhere else entirely.
	#
	# WALLCLOCK_REQUIRE_BPF turns the unprivileged skip into a failure, so
	# this target cannot pass by quietly doing nothing.
	@set -e; for dir in $(KERNEL_TEST_DIRS); do \
		name=$$(basename $$dir); \
		echo "==> $$dir"; \
		$(GO) test $(GO_BUILDFLAGS) -c -o "$(CURDIR)/build/$$name.test" ./$$dir; \
		( cd $$dir && $(SUDO) env WALLCLOCK_REQUIRE_BPF=1 "$(CURDIR)/build/$$name.test" -test.v ); \
	done
	$(SUDO) $(BIN) load $(firstword $(BPF_OBJ))
	$(SUDO) $(BIN) load $(firstword $(BPF_OBJ))

.PHONY: matrix
matrix: bpf ## Load and test on a range of kernels under QEMU (no root needed)
	# No sudo. /dev/kvm is world-readable on an ordinary desktop and the root
	# these tests need is root inside the guest, which vimto provides. The one
	# thing this does need is working KVM: without it QEMU falls back to
	# software emulation and the guests are slow enough to be indistinguishable
	# from hung, which is how the first attempt at this matrix failed seven CI
	# rounds running.
	bash scripts/kernel-matrix.sh

.PHONY: distro-matrix
distro-matrix: bpf ## Same, against the kernels distributions actually ship
	# Slower and heavier than `matrix`, and it answers something that one
	# cannot: the ci-kernels images are built without CONFIG_CFS_BANDWIDTH, so
	# they can prove the programs load and attach but can never throttle a
	# cgroup -- the category this tool exists to separate. These images can.
	bash scripts/distro-matrix.sh

FLAME_WINDOW ?= 20s

.PHONY: validate
validate: build ## Measure the tool against answers known in advance (needs root)
	# The validation table in the README, generated rather than transcribed.
	# It exits non-zero when a scenario lands outside its declared tolerance,
	# which is what makes the tolerances mean something: a number typed into
	# a README by hand is a claim about a run nobody can repeat.
	$(SUDO) $(BIN) validate

OVERHEAD_WINDOW  ?= 2s
OVERHEAD_REPEATS ?= 5

.PHONY: overhead
overhead: build ## Measure what the tool costs against event rate (needs root)
	# Takes a few minutes: eight rates, three runs each, repeated. Two of
	# those three runs are unprofiled -- one is the baseline and one is the
	# control that says how much two identical runs differ, which is the
	# resolution of the whole exercise.
	$(SUDO) $(BIN) overhead -for $(OVERHEAD_WINDOW) -repeats $(OVERHEAD_REPEATS)

COMPARE_WINDOW ?= 12

.PHONY: compare
compare: build ## Run wallclock, offcputime and runqlat on the same subjects (needs root)
	# The evidence behind "why not just use bcc-tools". Needs bpfcc-tools;
	# the script says so and stops if they are missing rather than producing
	# half a comparison.
	$(SUDO) env WINDOW=$(COMPARE_WINDOW) sh scripts/compare-tools.sh

.PHONY: flamegraph
flamegraph: build ## Record off-CPU stacks and render a flame graph (needs root)
	# An off-CPU flame graph, not the usual kind: the width of a frame is not
	# how long that code ran, it is how long a thread sat still inside it.
	#
	# Rendered by the tool itself rather than by flamegraph.pl. Two reasons,
	# and neither is not-invented-here: this is one command on a clean clone
	# with nothing to fetch, and the colour of a frame can be the reason the
	# thread stopped -- which this profiler classified on the way past and a
	# renderer given only a folded file cannot know. The folded file is
	# written as well, for anyone who wants to take it elsewhere.
	@mkdir -p docs
	$(SUDO) $(BIN) profile -for $(FLAME_WINDOW) \
		-folded build/offcpu.folded -flame docs/offcpu-flamegraph.svg
	# Written by root, into a directory the repository tracks. Handed back to
	# whoever owns the tree -- by reference rather than by `id -u`, because
	# this target is usually run as root outright, where `id -u` is 0 and the
	# chown would be a no-op that reads like a fix.
	@$(SUDO) chown --reference=Makefile docs/offcpu-flamegraph.svg build/offcpu.folded

.PHONY: clean
clean: ## Remove build outputs
	rm -rf build bin

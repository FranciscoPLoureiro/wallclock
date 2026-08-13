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
BPF_CFLAGS := -target bpf -D__TARGET_ARCH_x86 \
	-I/usr/include/$(shell uname -m)-linux-gnu \
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

.PHONY: bpf
bpf: $(BPF_OBJ) ## Compile the BPF programs

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
smoke: bpf build ## Load the compiled objects into this kernel (needs root)
	# Compiled first and elevated second, rather than running `sudo go test`.
	# sudo resets PATH to secure_path, which has no /usr/local/go/bin, and it
	# hands the build a different HOME and therefore a cold module cache. A
	# test binary needs neither.
	#
	# WALLCLOCK_REQUIRE_BPF turns the unprivileged skip into a failure, so
	# this target cannot pass by quietly doing nothing.
	#
	# WALLCLOCK_BPF_OBJECT because `go test` runs a test in its own package
	# directory and a compiled test binary runs wherever it was launched, so
	# the relative default in the test resolves to two different places.
	$(GO) test $(GO_BUILDFLAGS) -c -o build/bpfload.test ./internal/bpfload
	$(SUDO) env WALLCLOCK_REQUIRE_BPF=1 WALLCLOCK_BPF_OBJECT="$(CURDIR)/$(firstword $(BPF_OBJ))" \
		./build/bpfload.test -test.v
	$(SUDO) $(BIN) load $(firstword $(BPF_OBJ))

.PHONY: clean
clean: ## Remove build outputs
	rm -rf build bin

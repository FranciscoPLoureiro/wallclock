// Package preflight answers one question about a host: can wallclock run on
// it, and when it cannot, which requirement is missing and what that costs.
//
// Every check carries a consequence as well as a verdict, because "cgroup v2
// not mounted" only helps somebody who already knows that cgroup v2 is where
// the throttling numbers come from. Those consequences are also the source
// text for docs/COMPATIBILITY.md, kept next to the code so the document and
// the program cannot drift apart.
package preflight

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/asm"
	"github.com/cilium/ebpf/btf"
	"github.com/cilium/ebpf/rlimit"
	"golang.org/x/sys/unix"
)

// The minimum kernel this project supports. 5.8 is where BPF_MAP_TYPE_RINGBUF
// arrived; the ring buffer is the chosen transport for per-event streaming, so
// below this version the design is not degraded but absent.
const (
	minKernelMajor = 5
	minKernelMinor = 8
)

// Check is the outcome of a single host requirement.
type Check struct {
	// Name states the requirement rather than the test, so a passing report
	// reads as a list of facts about the host.
	Name string
	// OK reports whether the requirement is satisfied.
	OK bool
	// Detail is what was actually found. Always populated: evidence when the
	// check passes, the reason when it does not.
	Detail string
	// Consequence is what stops working when this requirement is missing.
	Consequence string
	// Optional marks a capability the tool runs without rather than a
	// requirement it cannot start without. A failing optional check costs one
	// column of the report and is not a reason to refuse the host -- but it
	// is still printed, because a column that is absent and a column that
	// measured zero are different claims and the report has to be able to
	// tell them apart.
	Optional bool
}

// Report is the result of every check, in a fixed order.
type Report struct {
	Checks []Check
}

// OK reports whether every requirement passed. Optional capabilities are not
// requirements and do not decide it.
func (r Report) OK() bool {
	return len(r.Failed()) == 0
}

// Failed returns the requirements that did not pass, in report order.
func (r Report) Failed() []Check {
	var failed []Check
	for _, c := range r.Checks {
		if !c.OK && !c.Optional {
			failed = append(failed, c)
		}
	}
	return failed
}

// Requirements counts the checks that decide whether the host is usable, which
// is every check that is not an optional capability.
func (r Report) Requirements() int {
	n := 0
	for _, c := range r.Checks {
		if !c.Optional {
			n++
		}
	}
	return n
}

// Degraded returns the optional capabilities this host does not have, in
// report order. The tool runs; something it would otherwise report does not.
func (r Report) Degraded() []Check {
	var degraded []Check
	for _, c := range r.Checks {
		if !c.OK && c.Optional {
			degraded = append(degraded, c)
		}
	}
	return degraded
}

// host locates the pseudo-filesystems the checks read. Tests point it at a
// fixture directory; everything else uses hostPaths.
type host struct {
	cgroupRoot string
}

func hostPaths() host {
	return host{cgroupRoot: "/sys/fs/cgroup"}
}

// Run executes every check against the running host.
//
// The last check loads a program into the kernel instead of inspecting
// capability bits, because the bits are a proxy and the load is the thing
// itself: a process can hold CAP_BPF and still be refused by seccomp, by an
// LSM, or by a sysctl. Attempting the operation cannot be wrong about whether
// the operation is permitted.
func Run() Report {
	h := hostPaths()
	return Report{Checks: []Check{
		checkKernelVersion(),
		checkBTF(),
		checkCgroupV2(h),
		checkCPUController(h),
		checkCFSBandwidth(h),
		checkProgramLoad(),
	}}
}

func checkKernelVersion() Check {
	c := Check{
		Name: "kernel >= 5.8",
		Consequence: "BPF_MAP_TYPE_RINGBUF does not exist before 5.8, so " +
			"per-event streaming has no transport",
	}

	var uts unix.Utsname
	if err := unix.Uname(&uts); err != nil {
		c.Detail = fmt.Sprintf("uname failed: %v", err)
		return c
	}
	release := unix.ByteSliceToString(uts.Release[:])

	major, minor, err := parseRelease(release)
	if err != nil {
		c.Detail = fmt.Sprintf("%s: %v", release, err)
		return c
	}
	c.OK = major > minKernelMajor || (major == minKernelMajor && minor >= minKernelMinor)
	c.Detail = fmt.Sprintf("%s (parsed as %d.%d)", release, major, minor)
	return c
}

// parseRelease extracts the leading major.minor from a uname release string.
//
// Everything after the minor number is unconstrained -- 6.6.87.2-microsoft-
// standard-WSL2, 5.10.0-28-amd64, 6.12.0-rc3+ -- so only the first two dotted
// numbers are read, and each stops at its first non-digit.
func parseRelease(release string) (major, minor int, err error) {
	if release == "" {
		return 0, 0, errors.New("empty release string")
	}

	leadingDigits := func(s string) (int, error) {
		end := 0
		for end < len(s) && s[end] >= '0' && s[end] <= '9' {
			end++
		}
		if end == 0 {
			return 0, fmt.Errorf("no leading digits in %q", s)
		}
		return strconv.Atoi(s[:end])
	}

	parts := strings.SplitN(release, ".", 3)
	if major, err = leadingDigits(parts[0]); err != nil {
		return 0, 0, fmt.Errorf("major version: %w", err)
	}
	if len(parts) == 1 {
		// A kernel built without a sublevel reports just "6". Reading that as
		// 6.0 is the conservative interpretation.
		return major, 0, nil
	}
	if minor, err = leadingDigits(parts[1]); err != nil {
		return 0, 0, fmt.Errorf("minor version: %w", err)
	}
	return major, minor, nil
}

func checkBTF() Check {
	c := Check{
		Name: "kernel BTF",
		Consequence: "without BTF there is no CO-RE: struct field offsets cannot " +
			"be relocated at load time, and every object would have to be recompiled " +
			"against the headers of each target kernel",
	}

	spec, err := btf.LoadKernelSpec()
	if err != nil {
		c.Detail = fmt.Sprintf("cannot load kernel BTF: %v", err)
		return c
	}

	// That the file exists proves little. Phase 2 reads fields out of
	// task_struct, so the check is that the blob parses and contains the type
	// this project will actually relocate against.
	var task *btf.Struct
	if err := spec.TypeByName("task_struct", &task); err != nil {
		c.Detail = fmt.Sprintf("BTF loaded but task_struct is missing: %v", err)
		return c
	}
	c.OK = true
	c.Detail = fmt.Sprintf("parses, task_struct has %d members", len(task.Members))
	return c
}

func checkCgroupV2(h host) Check {
	c := Check{
		Name: "cgroup v2",
		Consequence: "cgroup filtering and the cpu.stat cross-check both address " +
			"the unified hierarchy; under cgroup v1 wallclock can still report per-PID " +
			"time, but not per-container time",
	}

	var st unix.Statfs_t
	if err := unix.Statfs(h.cgroupRoot, &st); err != nil {
		c.Detail = fmt.Sprintf("statfs %s: %v", h.cgroupRoot, err)
		return c
	}
	if st.Type != unix.CGROUP2_SUPER_MAGIC {
		c.Detail = fmt.Sprintf("%s is not cgroup2 (magic 0x%x)", h.cgroupRoot, st.Type)
		return c
	}
	c.OK = true
	c.Detail = fmt.Sprintf("cgroup2fs at %s", h.cgroupRoot)
	return c
}

func checkCPUController(h host) Check {
	c := Check{
		Name: "cgroup cpu controller",
		Consequence: "no cpu controller means no cpu.max and no cpu.stat, so " +
			"throttling cannot be observed and cannot be told apart from ordinary " +
			"runqueue delay -- the distinction this tool exists to make",
	}

	path := filepath.Join(h.cgroupRoot, "cgroup.controllers")
	raw, err := os.ReadFile(path) //nolint:gosec // a fixed path under /sys
	if err != nil {
		c.Detail = fmt.Sprintf("read %s: %v", path, err)
		return c
	}
	available := strings.Fields(string(raw))
	for _, name := range available {
		if name == "cpu" {
			c.OK = true
			c.Detail = fmt.Sprintf("enabled at the root (%s)", strings.Join(available, " "))
			return c
		}
	}
	c.Detail = fmt.Sprintf("cpu absent from %s (found: %s)", path, strings.Join(available, " "))
	return c
}

// checkCFSBandwidth reports whether this kernel can throttle a cgroup at all.
//
// Read from cpu.stat rather than from /proc/kallsyms or a kernel config,
// because it is the interface the kernel actually exposes: a kernel built
// with CONFIG_CFS_BANDWIDTH accounts nr_periods, nr_throttled and
// throttled_usec in every cgroup's cpu.stat, and one built without it accounts
// none of them anywhere. /proc/config.gz is absent on plenty of hosts, and a
// missing symbol cannot tell a feature that was compiled out apart from one
// the compiler inlined -- which is the distinction the whole decision rests
// on.
//
// This is optional rather than required. Where the kernel cannot throttle,
// throttled time is not unknown, it is necessarily zero, so running without
// the throttling probes cannot misfile anything -- there is nothing to
// misfile. That is the opposite of the case where the kernel does account
// bandwidth and the probe still will not attach: there the time exists,
// carrying on would file it as ordinary runqueue delay, and that is the one
// confusion this tool was built to remove. offcpu.Open makes that distinction
// with this same signal and refuses in the second case.
func checkCFSBandwidth(h host) Check {
	c := Check{
		Name:     "cgroup cpu bandwidth",
		Optional: true,
		Consequence: "this kernel was built without CONFIG_CFS_BANDWIDTH, so no " +
			"cgroup can be throttled and there is no throttle_cfs_rq to attach to; " +
			"every other column is measured as usual and throttled is reported as " +
			"unavailable rather than as a measured zero",
	}

	path := filepath.Join(h.cgroupRoot, "cpu.stat")
	raw, err := os.ReadFile(path) //nolint:gosec // a fixed path under /sys
	if err != nil {
		c.Detail = fmt.Sprintf("read %s: %v", path, err)
		return c
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(line, "throttled_usec ") {
			c.OK = true
			c.Detail = fmt.Sprintf("%s accounts throttled_usec", path)
			return c
		}
	}
	c.Detail = fmt.Sprintf("%s accounts no throttled_usec, so the kernel has no "+
		"CFS bandwidth control", path)
	return c
}

// ThrottlingObservable reports whether this kernel accounts for CFS bandwidth,
// and therefore whether attaching the throttling probes can succeed.
//
// Exported so that offcpu can ask the same question this report answers,
// against the same file, rather than growing a second copy of the rule that
// would eventually be a different rule.
func ThrottlingObservable() bool {
	return checkCFSBandwidth(hostPaths()).OK
}

func checkProgramLoad() Check {
	c := Check{
		Name: "load a tracepoint program",
		Consequence: "without CAP_BPF and CAP_PERFMON (in practice: root) no " +
			"program loads at all and wallclock cannot run",
	}

	// Before 5.11 the kernel charges BPF memory against RLIMIT_MEMLOCK, whose
	// default is small enough to reject real maps. From 5.11 the allocation is
	// charged to the memory cgroup instead and this is unnecessary.
	//
	// Its error is deliberately not fatal to the check. Raising the limit
	// needs CAP_SYS_RESOURCE, so an unprivileged process fails here first and
	// the report would then blame a memory limit for what is a capability
	// problem -- a diagnosis that sends the reader to /etc/security/limits.conf
	// to fix something that was never wrong. The load below gives the real
	// answer, and this only gets mentioned if it turns out to be relevant.
	memlockErr := rlimit.RemoveMemlock()

	prog, err := ebpf.NewProgram(&ebpf.ProgramSpec{
		Name: "wallclock_pre",
		Type: ebpf.TracePoint,
		Instructions: asm.Instructions{
			asm.Mov.Imm(asm.R0, 0),
			asm.Return(),
		},
		License: "Dual BSD/GPL",
	})
	if err != nil {
		c.Detail = fmt.Sprintf("rejected: %v", err)
		if memlockErr != nil {
			c.Detail += fmt.Sprintf(" (RLIMIT_MEMLOCK could not be raised either: %v)", memlockErr)
		}
		return c
	}
	defer prog.Close()

	c.OK = true
	c.Detail = "two-instruction program accepted by the verifier"
	return c
}

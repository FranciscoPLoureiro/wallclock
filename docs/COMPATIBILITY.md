# Compatibility

What wallclock needs from a host, how each requirement is checked, and what
happens when it is missing. `wallclock preflight` runs every check in this
document and prints the same consequences, so the two cannot drift: the text
below is the same text the program prints, and both come from
[`internal/preflight`](../internal/preflight/preflight.go).

```
$ sudo wallclock preflight
requirement                status  found
kernel >= 5.8              ok      7.1.9-arch1-2 (parsed as 7.1)
kernel BTF                 ok      parses, task_struct has 267 members
cgroup v2                  ok      cgroup2fs at /sys/fs/cgroup
cgroup cpu controller      ok      enabled at the root (cpuset cpu io memory hugetlb pids rdma misc dmem)
cgroup cpu bandwidth       ok      /sys/fs/cgroup/cpu.stat accounts throttled_usec
load a tracepoint program  ok      two-instruction program accepted by the verifier

all requirements met
```

It exits non-zero if anything fails, so it can gate a build or a container
start.

## The requirements

| Requirement | Checked by | Missing means |
|---|---|---|
| Linux **5.8** or newer | `uname` release, parsed to major.minor | `BPF_MAP_TYPE_RINGBUF` does not exist, so per-event streaming has no transport |
| Kernel **BTF** | loading `/sys/kernel/btf/vmlinux` and looking up `task_struct` | no CO-RE: field offsets cannot be relocated at load time, and every object would have to be recompiled against the headers of each target kernel |
| **cgroup v2** | `statfs` magic of `/sys/fs/cgroup` | cgroup filtering and the `cpu.stat` cross-check both address the unified hierarchy; per-PID time still works, per-container time does not |
| cgroup **cpu controller** | `cpu` present in `/sys/fs/cgroup/cgroup.controllers` | no `cpu.max` and no `cpu.stat`, so throttling cannot be observed and cannot be told apart from ordinary runqueue delay |
| **CAP_BPF** and **CAP_PERFMON** | loading a two-instruction tracepoint program | nothing loads at all |

One further check is reported and is not a requirement:

| Capability | Checked by | Absent means |
|---|---|---|
| cgroup **cpu bandwidth** | `throttled_usec` present in `/sys/fs/cgroup/cpu.stat` | the kernel was built without `CONFIG_CFS_BANDWIDTH`, so nothing can be throttled and `throttle_cfs_rq` does not exist to attach to. Every other column is measured as usual and `throttled` is reported as **unavailable** rather than as a measured zero |

The distinction is the whole reason it is not a requirement. Where the kernel
cannot throttle, throttled time is not unknown, it is necessarily zero, and
running without those probes cannot misfile anything because there is nothing
to misfile. Where the kernel *does* account bandwidth and the probe still will
not attach, `offcpu.Open` refuses instead: there the time is real, and carrying
on would file it as ordinary runqueue delay -- the one confusion this tool
exists to remove.

### Why 5.8 and not lower

Two things arrived in 5.8 and this project needs both.

`BPF_MAP_TYPE_RINGBUF` is the transport for the per-event path. The older
alternative, the perf buffer, is per-CPU and does not order events across
CPUs — and correlating a `sched_wakeup` on one CPU with the `sched_switch`
that follows it on another is precisely the work phase 2 does.

5.8 is also where `CAP_BPF` and `CAP_PERFMON` were split out of
`CAP_SYS_ADMIN`. Below it, loading any BPF program means full
`CAP_SYS_ADMIN`, which is close enough to root that a container running
wallclock would have to be trusted with everything else too.

Individual features used later arrived later still, and each will be gated
where it is introduced rather than by raising the floor here: `bpf_loop()` is
5.17, and BPF trampolines — `fentry`/`fexit`, cheaper than kprobes for the
scheduler hooks — are 5.5 and therefore already covered.

### Why the capability check loads a program

Reading `CapEff` out of `/proc/self/status` answers a different question than
the one that matters. A process can hold `CAP_BPF` and still be refused by
seccomp, by an LSM, or by `kernel.unprivileged_bpf_disabled`; it can also
succeed without holding what a naive reading of the documentation suggests.
Attempting the operation cannot be wrong about whether the operation is
permitted.

The probe is a tracepoint program rather than a socket filter because that is
the class every program in this project belongs to. Loading a socket filter
needs only `CAP_BPF`; a tracepoint needs `CAP_PERFMON` as well, and proving
the weaker requirement would prove the wrong thing.

### RLIMIT_MEMLOCK

Before 5.11 the kernel charged BPF memory against `RLIMIT_MEMLOCK`, whose
default is small enough to reject anything past a toy map. From 5.11 it is
charged to the memory cgroup instead and the limit is irrelevant.

wallclock raises it on a best-effort basis and does not treat the failure as
an answer. Raising it needs `CAP_SYS_RESOURCE`, so an unprivileged process
fails there *before* reaching the load — and a report that blamed a memory
limit would send the reader to `limits.conf` to fix something that was never
wrong. The load below it gives the real answer.

## Running it

In practice: **root**, or a container with `CAP_BPF` and `CAP_PERFMON`.

```bash
sudo wallclock preflight
```

Under Docker, `--privileged` works and is heavier than necessary. The
narrower form is `--cap-add BPF --cap-add PERFMON`, plus access to the
tracing filesystem for attaching; a container that also needs to read the
host's cgroups needs `/sys/fs/cgroup` mounted.

An unprivileged run fails on the last check alone, and says so:

```
load a tracepoint program  FAIL    rejected: load program: operation not permitted
                                   (RLIMIT_MEMLOCK could not be raised either: ...)

load a tracepoint program is not satisfied.
  without CAP_BPF and CAP_PERFMON (in practice: root) no program loads at
  all and wallclock cannot run
```

## Where this has been verified

Two matrices, both reproducible with one command, plus CI on every pull
request. `make matrix` boots kernel images under QEMU; `make distro-matrix`
boots the cloud images distributions publish. Neither needs root on the host:
`/dev/kvm` is world-readable on an ordinary desktop and the root these tests
need is root inside the guest.

**Five kernels run in CI on every pull request** -- 5.4, 5.15, 6.1, 6.6 and
stable, spanning the floor -- so the claim is enforced rather than recorded.
That job took seven attempts to get working and the cause was never the tool:
GitHub's runners carry `/dev/kvm` and do not make it readable by the runner
user, QEMU falls back to software emulation, and a guest thirty times slower
is indistinguishable from a hung one. The job now installs a udev rule and
then *asserts* the accelerator is usable, because a missing accelerator has to
be a named failure rather than a timeout. The distribution images stay local:
they are hundreds of megabytes each, and they are the ones that exercise
throttling.

### Kernels a distribution ships

These are the interesting ones. They are what a reader would run this on, they
are built with the configs distributions actually use, and they can therefore
exercise cgroup throttling -- which the BPF CI images cannot.

| Kernel | Distribution | `task_struct` | Result |
|---|---|---|---|
| `5.4.0-216-generic` | Ubuntu 20.04 | no BTF | refused, correctly: below the 5.8 floor |
| `5.15.0-190-generic` | Ubuntu 22.04 | 243 members | every suite, nothing skipped |
| `6.1.0-52-cloud-amd64` | Debian 12 | 244 members | every suite, nothing skipped |
| `6.8.0-138-generic` | Ubuntu 24.04 | 273 members | every suite, nothing skipped |
| `5.14.0-687.10.1.el9_8` | Rocky 9 | 262 members | every suite, nothing skipped |
| `7.1.9-arch1-2` | Arch, bare metal | 267 members | every suite, nothing skipped |
| `6.17.0-*-azure` | GitHub Actions, CI | 266 members | preflight and `make smoke`, per pull request |

### Kernels from the BPF CI images

`ghcr.io/cilium/ci-kernels`, which is how cilium/ebpf -- the library this is
built on -- runs its own tests. They span further than any distribution does
and they cost seconds each.

| Kernel | `task_struct` | Result |
|---|---|---|
| `4.19.322` | no BTF | refused, correctly |
| `5.4.296` | no BTF | refused, correctly |
| `5.10.259` | 198 members | passes, throttling skipped |
| `5.15.210` | 212 members | passes, throttling skipped |
| `6.1.176` | 224 members | passes, throttling skipped |
| `6.6.143` | 232 members | passes, throttling skipped |
| `6.10.13` | 228 members | passes, throttling skipped |
| `7.1.1` | 229 members | passes, throttling skipped |

Every one of those images is built with `CONFIG_CFS_BANDWIDTH` unset, so no
cgroup in them can be throttled and the throttling test is skipped rather than
run. That is worth stating plainly: **the QEMU matrix alone could never have
validated the claim this project is about**, whatever else had gone right with
it. It proves loading, attachment, and the decomposition; the category that
distinguishes this tool from `offcputime` needs the distribution images.

### What the two together are evidence of

Thirteen kernels, one compiled object, `task_struct` measured at eight
different sizes between 198 and 273 members. The pairs are the argument:

| Same upstream version, different build | `task_struct` |
|---|---|
| 5.15.210 (BPF CI) against `5.15.0-190` (Ubuntu 22.04) | 212 against 243 |
| 6.1.176 (BPF CI) against `6.1.0-52` (Debian 12) | 224 against 244 |

Thirty-one members apart on the same nominal kernel. That is CO-RE stated as a
measurement rather than as a principle, and it is a different statement from
two kernels eleven minor versions apart, both above 6.6, where nothing had
moved.

### The floor is measured now, not reasoned

4.19, 5.4 under QEMU and Ubuntu 20.04's `5.4.0-216` all refuse, and the
assertion is not that they failed. Nearly everything that goes wrong also
fails. It is that the run reached its end -- the guest prints a sentinel after
the work, and a row whose log lacks it is recorded as *no result* rather than
as a refusal -- and that the kernel requirement is the one that refused.

That check exists in that shape because of how the previous attempt failed. A
refusal test passed on a hang: the deadline killed QEMU with status 124, the
pattern matching the refusal accepted the bare word "kernel", and that word
appears in the timeout message. Green, with the tool never having run.

## What the matrix found

Building it was worth more than the rows it produced.

### Red Hat kernels were being reported wrongly, and silently

On Rocky 9 the tool attached to every tracepoint without complaint and then
reported thread ids of 6911073 and 7102830, with an empty command column, a
decomposition closing to 100%, and `no threads lost`.

Red Hat's 9.x kernel calls itself 5.14 and carries PREEMPT_LAZY, which is
upstream in 6.13. The backport adds `common_preempt_lazy_count` to the common
header every tracepoint record begins with, and everything after it moves --
by four bytes for the four-byte fields, and by eight for `sched_switch`'s
`prev_state`, which is eight bytes and has to be aligned:

```
                Arch 7.1.9    Rocky 9 (5.14.0-687)
prev_comm            8             12
prev_pid            24             28
prev_state          32             40
next_pid            56             64
```

The programs read those fields at offsets fixed when they were compiled, so
`prev_pid` came from the last four bytes of `prev_comm`.

This project had already met that hazard once, on `sched_process_fork`, and
the comment recording it in `bpf/offcpu.bpf.c` states the rule correctly:
tracepoint field *names* are stable ABI and their offsets are not. That case
announced itself, because the offsets moved *past the end* of the record and
the kernel refuses to attach a program that reads beyond a tracepoint's size.
This one did not: the record grew, every read stayed inside it, and the
program ran.

**A read that goes out of bounds is caught by the kernel. A read that lands on
the wrong field of the right record is caught by nothing** -- and the two
integrity claims this tool makes about its own output, that the decomposition
closes and that no threads were lost, were both perfectly true of the wrong
threads.

`internal/tracefs` now reads the layout from the kernel that is about to load
the program and writes the real offsets into the programs before they load. A
field that is missing, or that kept its name and changed width, is an error
naming the field and both offsets rather than a default to fall back on --
because falling back is precisely the behaviour that produced the wrong
numbers. The matrix records each kernel's layout in its log, so a future
divergence is visible rather than inferred.

### The test suite only ran fully on Debian

Three places copied `/bin/dash` to make a CPU-burning subject: the throttling
test, `wallclock validate`, and `scripts/compare-tools.sh`. Debian and Ubuntu
ship that file and Arch, Fedora and Red Hat do not. On any other distribution
the throttling test -- the claim this project exists to make -- **skipped**,
and the suite reported green. `internal/spin` resolves a shell from a
candidate list ending at `/bin/sh`, which POSIX requires, and a host without
one is now a failure rather than a skip.

### A test was asserting on a symbol rather than on behaviour

`TestABlockedSocketReadIsClassifiedAsNetwork` required a stack frame named
`ping_recvmsg` or `inet_recvmsg`. `inet_recvmsg` is inlined away on 7.1, and
which of the two appears at all depends on whether `ping` got a raw socket or
an ICMP datagram socket, which depends on whether it was running as root. It
failed on a kernel where the classification was entirely correct. It now
matches the tid of the `ping` it started, which is both stricter about whose
wait it is looking at and indifferent to what the compiler did.

## Known gaps

- **arm64 does not build, and here is where it stops.** Compiling the
  programs with `-D__TARGET_ARCH_arm64` gets two of the four through: the
  tracepoint-only ones, `minimal` and `syscount`. The two that use kprobes,
  `offcpu` and `netlat`, fail at `BPF_KPROBE` with *incomplete definition of
  type 'const struct user_pt_regs'* -- libbpf's `bpf_tracing.h` needs arm64's
  own `pt_regs` layout, which is not on an x86-64 host and does not come from
  the target flag. It wants arm64 kernel headers or a `vmlinux.h` generated
  from an arm64 BTF. That is a build problem rather than a design one, and
  compiling is anyway a long way short of loading: nothing here says the
  verifier on an arm64 kernel would accept the result.
- **The build has a Debian assumption in it.** `-I/usr/include/$(uname
  -m)-linux-gnu` is a multiarch path that exists on Debian and Ubuntu and
  nowhere else. It is harmless elsewhere -- clang ignores an include directory
  that is not there, and other distributions put `asm/types.h` where it is
  already looked for -- but the comment above it describes it as necessary,
  and it is necessary on one family.
- **cgroup v1 is not supported**, and the tool refuses to start rather than
  reporting numbers that mean something different.
- **No kernel between 4.19 and 5.4 has been tried**, and the floor is 5.8. The
  two below it refuse for two independent reasons, version and missing BTF, so
  nothing here separates those two causes.

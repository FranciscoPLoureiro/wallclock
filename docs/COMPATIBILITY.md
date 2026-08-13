# Compatibility

What wallclock needs from a host, how each requirement is checked, and what
happens when it is missing. `wallclock preflight` runs every check in this
document and prints the same consequences, so the two cannot drift: the text
below is the same text the program prints, and both come from
[`internal/preflight`](../internal/preflight/preflight.go).

```
$ sudo wallclock preflight
requirement                status  found
kernel >= 5.8              ok      6.6.87.2-microsoft-standard-WSL2 (parsed as 6.6)
kernel BTF                 ok      parses, task_struct has 240 members
cgroup v2                  ok      cgroup2fs at /sys/fs/cgroup
cgroup cpu controller      ok      enabled at the root (cpuset cpu io memory hugetlb pids rdma)
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

Both of these are checked by the same command, and the CI one runs on every
pull request.

| Host | Kernel | BTF | cgroup | clang | Result |
|---|---|---|---|---|---|
| WSL2, Debian 13 (development) | `6.6.87.2-microsoft-standard-WSL2` | 6 050 732 bytes, `task_struct` 240 members | v2, `cpu` delegated | 19.1.7 | all requirements met |
| GitHub Actions `ubuntu-latest` (CI) | `6.17.0-1022-azure` | 6 841 206 bytes, `task_struct` 266 members | v2, `cpu` delegated | 18.1.3 | all requirements met |

The two kernels differ by eleven minor versions and report `task_struct` with
a different number of members — which is the argument for CO-RE stated as a
measurement rather than as a principle. See the README for that decision. The
two clang versions differ as well, which is a second thing this pair happens
to hold constant only because it is checked.

**The runner kernel moves without notice.** Two runs a few hours apart landed
on `6.17.0-1022-azure` and `6.17.0-1020-azure`. Nothing broke, and that is the
point of recording it in the job summary: the day something does break, the
first question is what changed, and "the image" is only a satisfying answer
when the previous value was written down.

## Known gaps

- **Only x86-64 is exercised.** The BPF objects are compiled with
  `-D__TARGET_ARCH_x86` and the include path is resolved from `uname -m`.
  arm64 should follow from changing both, and has not been tried.
- **No second kernel in CI.** A hosted runner gives one kernel. Testing a
  second means QEMU with pre-built kernel images, which is what the upstream
  BPF CI does; until then, compatibility below 6.6 is reasoned from the
  feature-to-version map above and not measured.
- **cgroup v1 is not supported and is not detected gracefully beyond the
  check.** The tool refuses to start rather than silently reporting numbers
  that mean something different.

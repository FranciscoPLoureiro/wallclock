# wallclock

[![CI](https://github.com/FranciscoPLoureiro/wallclock/actions/workflows/ci.yml/badge.svg)](https://github.com/FranciscoPLoureiro/wallclock/actions/workflows/ci.yml)

A wall-clock latency profiler for Linux, in eBPF. It takes a PID, a cgroup or
a container and splits the time that passed into where it actually went: on
CPU, ready but waiting for a CPU, ready but stopped by its own cgroup quota,
blocked on the network per destination, blocked on a lock, blocked on disk.
The application is not recompiled, restarted or instrumented.

This is what it is being built to print:

```
process: ticketoffice-api  (pid 4412, 14 threads, cgroup /docker/a3f1…)
window: 30s

  on-cpu                       18.3%   ████
  runqueue (ready, no CPU)     28.4%   ██████
  throttled (quota exhausted)  13.3%   ███
  blocked on network           22.1%   █████
    -> 10.5.0.3:6379  (redis)          16.8%
    -> 10.5.0.4:5432  (postgres)        4.1%
    -> 10.5.0.5:5672  (rabbitmq)        1.2%
  blocked on futex             14.2%   ███
  blocked on disk               2.1%
  other                         1.6%
                              ------
                              100.0%
```

**None of that exists yet.** The block above is the target, not a screenshot;
it is here because the shape of the answer is the design, and because the two
lines that matter — *runqueue* and *throttled* — are the reason the project
exists rather than a nicety. Phase 0 is done and it is the environment, the
skeleton and the proof that the pipeline can work at all. What runs today is
at [what works now](#what-works-now).

**The percentages summing to 100% is the point.** Existing tools report loose
quantities that do not compose: `offcputime` gives off-CPU stacks without
separating *blocked* from *ready-but-starved*, `runqlat` gives a distribution
of runqueue latency without saying whose or why, and neither knows anything
about a cgroup's quota. A closed decomposition of one process's wall clock in
one window, with throttling separated from runqueue delay, is the thing none
of them produce.

## Status

| Phase | Scope | State |
|---|---|---|
| 0 | Environment, repository, CI that loads BPF into a real kernel | ✅ done |
| 1 | First program end to end: map aggregation and a ring buffer path | planned |
| 2 | Off-CPU, runqueue delay, and cgroup throttling separated | planned |
| 3 | What the Go runtime makes invisible from the kernel's side | planned |
| 4 | Network time attributed to individual destinations | planned |
| 5 | Validation against injected latency, and measured overhead | planned |
| 6 | Point it at a real service and answer a real question | planned |

The question in phase 6 comes from the project this one grew out of:
[High Concurrency Ticket Office](https://github.com/FranciscoPLoureiro/HighConcurrencyTicketOffice),
whose README observes that p99 went from 153 ms to 291 ms when Prometheus and
Grafana started sharing the machine, and cannot say where the difference went.
That is a correlation. This is the tool that turns it into an explanation.

## What works now

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

```
$ sudo wallclock load build/minimal.bpf.o
loaded wallclock_minimal (TracePoint, 2 instructions)
  verification time 28 usec
  stack depth 0
  processed 2 insns (limit 1000000) max_states_per_insn 0 total_states 0 peak_states 0 mark_read 0
```

The last check in `preflight` loads a program instead of reading capability
bits, because the bits are a proxy and the load is the thing itself. The full
list of requirements, and what breaks when each is missing, is in
[docs/COMPATIBILITY.md](docs/COMPATIBILITY.md).

## Quick start

Needs a Linux host — see [compatibility](docs/COMPATIBILITY.md) — and root for
anything that touches the kernel.

```bash
sudo bash scripts/dev-env.sh
```

That installs the pinned toolchain: clang and llvm for the BPF objects,
libbpf's headers, Go 1.26.5 from upstream against a recorded checksum, and
golangci-lint. Then:

| Command | Does |
|---|---|
| `make verify` | Everything CI runs: clang, `go vet`, the linter, the tests |
| `make bpf` | Compile the BPF programs |
| `make build` | Build the `wallclock` binary |
| `make preflight` | Check this host against the requirements |
| `make smoke` | Load the compiled objects into this kernel (needs root) |
| `make test` | Tests with the race detector |

`make verify` runs as an ordinary user and the tests that need a kernel skip.
`make smoke` is the one that refuses to skip them; see below for why that
distinction is load bearing.

## CI loads the programs into a real kernel

A pipeline that only compiles proves nothing. clang will happily produce an
object the kernel refuses — bounded loops, a 512-byte stack, an instruction
ceiling, and a proof that every pointer was checked before it was read are the
verifier's rules, not the compiler's — and none of that is visible until the
load.

So on every pull request, a job compiles the programs, loads them into the
runner's kernel, and attaches one:

```
uname -r        6.17.0-1022-azure

requirement                status  found
kernel >= 5.8              ok      6.17.0-1022-azure (parsed as 6.17)
kernel BTF                 ok      parses, task_struct has 266 members
cgroup v2                  ok      cgroup2fs at /sys/fs/cgroup
cgroup cpu controller      ok      enabled at the root (cpuset cpu io memory hugetlb pids rdma misc dmem)
load a tracepoint program  ok      two-instruction program accepted by the verifier

all requirements met

--- PASS: TestObjectLoadsAndAttaches (0.04s)
    verifier: processed 2 insns (limit 1000000) ...
```

That a hosted runner has BTF and permits `bpf()` under sudo was an assumption
the whole plan rested on. It was checked on the first day rather than trusted
for three months, because if it were false every phase after this one would
need a different CI strategy.

**The test refuses to skip.** Loading a program needs root, so these tests
skip for an ordinary user — and a test that skips in CI proves exactly as much
as no test at all while still colouring the run green. `WALLCLOCK_REQUIRE_BPF=1`
turns the skip into a failure, and CI sets it. A runner that could not load
anything would go red rather than quietly report success over a suite that did
nothing.

## Decisions

The format is deliberate: context, the options actually considered, the
decision, and what it costs.

### CO-RE, not BCC

**Context.** A BPF program that reads a field out of a kernel struct — and
this one will read many, starting with `task_struct` — has to know that
field's byte offset. Offsets move between kernel versions and between build
configurations: a field added in the middle, a config option that includes or
excludes a member, and everything after it shifts. A program compiled against
one kernel's headers is wrong on another, and wrong in the worst way, since it
reads a valid address containing a different field.

**Options.** BCC compiles the program on the target machine at run time,
against that machine's headers, which makes the offsets right by construction.
CO-RE — Compile Once, Run Everywhere — compiles ahead of time and emits
*relocations*: the object records "this access is to the field named `pid`
inside `task_struct`" instead of "load 8 bytes at offset 2464". At load time
the loader reads the target kernel's BTF, resolves each relocation against the
type information the running kernel describes itself with, and patches the
offsets before handing the program to the verifier.

**Decision.** CO-RE, with `cilium/ebpf` as the loader.

BCC's model costs a full clang and LLVM on every target machine — hundreds of
megabytes — plus the kernel headers, plus compilation on every start, which is
seconds of CPU and a page cache full of headers on a machine that is being
profiled *because* it is short of CPU. It also moves compilation failures to
run time on someone else's host, which is the worst place to find them.

The measurement that makes this concrete is already in this repository. The
same binary runs against `6.6.87.2-microsoft-standard-WSL2` locally and
`6.17.0-1022-azure` in CI, and those two kernels report `task_struct` with 240
and 266 members respectively. Nothing was recompiled between them.

**Consequences.** CO-RE requires BTF on the target — `CONFIG_DEBUG_INFO_BTF=y`
and `/sys/kernel/btf/vmlinux` — which is why that is a hard requirement rather
than a nicety, and why the preflight check parses the BTF and looks for
`task_struct` instead of stat-ing the file. On a kernel built without it, the
fallback is BTFHub-style external BTF blobs, which is a real answer and one
this project does not implement. Field accesses must also go through the CO-RE
macros rather than plain C dereferences; a plain one compiles, loads, and
reads the wrong bytes, which is the failure mode this decision exists to
prevent and the reason it is written down before the first such access is
written.

### Why not just use bcc-tools or bpftrace

**Context.** `offcputime`, `runqlat`, `runqslower`, `wakeuptime`, `tcpconnlat`
have existed for years and each does part of this. Not having an answer to
this ends the conversation.

**Decision.** They are not composable into what this produces. `offcputime`
aggregates off-CPU stacks but does not separate *blocked on something* from
*ready and starved of CPU*, and knows nothing about cgroup quota. `runqlat`
gives a histogram of runqueue latency without saying whose, or why, or whether
the delay was the machine being full or the cgroup being throttled — which are
opposite problems with opposite fixes, a bigger machine against a bigger
limit. Running both does not fix it either: the windows do not coincide, the
categories overlap, and neither closes to 100%.

**Consequences.** This claim needs evidence, not assertion: the plan is to run
`offcputime` and `runqlat` against the same process as wallclock and put the
outputs side by side in this README. That belongs to phase 2, when there is an
output to compare against, and it is a deliverable rather than an aside.

### Where this is developed, and why not on Windows

**Context.** The host is Windows 11. eBPF needs a Linux kernel with BTF, a
cgroup v2 hierarchy with the `cpu` controller, and root. The previous project
worked around Windows Smart App Control blocking the Go toolchain by running
Go in a container; that constraint had to be re-established here rather than
inherited.

**Options.** WSL2, a full Linux VM (Hyper-V, Multipass, VirtualBox), a small
cloud VM, or the Docker Desktop VM.

**Decision.** WSL2 with Debian 13, verified rather than assumed. Everything
the project needs was measured before a line was written:

| | |
|---|---|
| kernel | `6.6.87.2-microsoft-standard-WSL2` — above the 5.8 floor |
| BTF | `/sys/kernel/btf/vmlinux`, 6 050 732 bytes, parses |
| `CONFIG_CFS_BANDWIDTH` | `=y` — cgroup throttling is observable, which phase 2 depends on |
| cgroup v2 | `cgroup2fs`, controllers `cpuset cpu io memory hugetlb pids rdma` |
| scheduler tracepoints | `sched_switch`, `sched_wakeup`, `sched_wakeup_new` present |
| `throttle_cfs_rq` / `unthrottle_cfs_rq` | both in `/proc/kallsyms` |
| `sch_netem.ko` | present, so phase 5 can inject known delay |
| loading a program | proven, with a two-instruction program through the raw `bpf()` syscall |

The Docker Desktop VM was rejected because it runs the *same* kernel,
`6.6.87.2`, so it offers nothing WSL2 does not, in an environment nobody
controls. A separate VM was not needed: everything the project requires is
already present, and the Smart App Control problem does not follow the
toolchain into Linux — gcc, clang and the Go toolchain all run unimpeded
inside the distribution.

**Consequences.** The working tree lives on the WSL2 ext4 filesystem rather
than under the Windows user profile, which is not a preference. The same
small-file workload a Go or clang build produces — 300 files written and read
back — takes **57 ms** on ext4 and **4 002 ms** across the `/mnt/c` 9p mount,
a factor of seventy, before counting a cloud sync client that would otherwise
be replicating build artefacts and `.git` as they change. The tree stays
reachable from Windows through `\\wsl.localhost\`, so it is one working copy
seen from two sides rather than two copies that drift.

The kernel is Microsoft's, not a distribution kernel, which is a real
limitation: it is one data point for compatibility and it is not the kernel
anything runs in production. CI on a stock Azure kernel is the second data
point, and the gap is recorded in
[compatibility](docs/COMPATIBILITY.md#known-gaps).

### The environment checks attempt the operation instead of inspecting for it

**Context.** "Can this machine run wallclock" can be answered by reading
`CapEff` from `/proc/self/status`, checking `kernel.unprivileged_bpf_disabled`,
and stat-ing `/sys/kernel/btf/vmlinux`.

**Decision.** Do the thing instead. `preflight` loads a two-instruction
tracepoint program; the BTF check parses the blob and looks up `task_struct`
rather than confirming a file exists.

Every inspection is a proxy for the operation, and proxies are wrong in both
directions: a process can hold `CAP_BPF` and still be refused by seccomp, by
an LSM, or by a sysctl, and a BTF file can exist and fail to parse. The
attempt cannot be wrong about whether the attempt succeeds.

**Consequences.** `preflight` has a side effect — it loads a program and
immediately closes it — which is a strange property for a diagnostic and is
the price of the answer being true. It also has to be run as root to be
conclusive, so an unprivileged run reports a failure that is really "ask
again with privileges"; the consequence text says so rather than leaving the
reader to work it out.

## Requirements

Linux 5.8+, kernel BTF, cgroup v2 with the `cpu` controller, and `CAP_BPF` +
`CAP_PERFMON` — in practice root or a container granted both. Full detail,
including what each missing piece costs and where this has been verified, in
[docs/COMPATIBILITY.md](docs/COMPATIBILITY.md).

## Licence

MIT. See [LICENSE](LICENSE).

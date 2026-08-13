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
| 1 | First program end to end: map aggregation and a ring buffer path | ✅ done |
| 2 | Off-CPU, runqueue delay, and cgroup throttling separated | 🔨 on-CPU, runqueue and blocked done; throttling next |
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

Wall clock split into where it actually went. Forty-eight spinning threads on
sixteen CPUs, so each one can have a third of a CPU and no more:

```
$ sudo wallclock profile -for 8s -comm wc-load -top 5
watching every thread for 8s

     tid  observed       on-cpu     runqueue  blocked  unknown  command
  100047     8.05s  31.1% 2.51s  68.9% 5.54s  0.0% 0s  0.0% 0s  wc-load
  100122     8.05s  31.6% 2.54s   68.4% 5.5s  0.0% 0s  0.0% 0s  wc-load
  100076     8.04s  31.7% 2.54s  68.3% 5.49s  0.0% 0s  0.0% 0s  wc-load
  100129     8.05s  31.8% 2.56s  68.2% 5.49s  0.0% 0s  0.0% 0s  wc-load
  100134     8.02s  31.6% 2.53s  68.4% 5.49s  0.0% 0s  0.0% 0s  wc-load

48 threads observed (5 shown)
every decomposition sums to 100% of the time observed
no threads lost
```

**16 CPUs ÷ 48 runnable threads = 33.3% of a CPU each.** Measured: 31.6%.
The remaining 1.7 points are the rest of the machine — the profiler included —
competing for the same cores. These threads never sleep, so their blocked
column is zero and every one of them spends two thirds of its life *ready and
waiting for a CPU*, which is the quantity a CPU profiler cannot see and an
off-CPU profiler reports as indistinguishable from waiting on a socket.

Syscall entries per process, counted inside the kernel, filtered inside the
kernel, with what was thrown away reported rather than assumed:

```
$ sudo wallclock syscount -for 5s -top 6
watching every process for 5s, counting in the kernel

    pid  entries  command
    479     2635  containerd
    539     2552  dockerd
    392     2169  initd
    881      653  containerd-shim
    295      648  Relay(9)
  88969      364  systemd-udevd

9776 entries across 66 processes (6 shown)
no events lost
```

The same tracepoint, streamed event by event through a ring buffer instead:

```
$ sudo wallclock syscount -for 2s -stream -pid 539
watching pid 539 for 2s, streaming every event

     since boot  pid    tid  syscall  command
  24714.871095s  539    541      204  dockerd
  24714.871203s  539    541       17  dockerd
  24714.871234s  539    541       35  dockerd
```

Counting syscalls is not interesting. Crossing the whole pipeline once with
the simplest possible problem is: C compiled to BPF, loaded past the verifier,
attached to a tracepoint, filtered in the kernel, and read back. Phase 2
attaches to the scheduler, where the difficulty should be the subject rather
than the tools.

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
| `make generate` | Regenerate the bpf2go bindings and the object they embed |
| `make build` | Build the `wallclock` binary |
| `make preflight` | Check this host against the requirements |
| `make smoke` | Run every test that needs a real kernel (needs root) |
| `make test` | Tests with the race detector |

The compiled BPF object is embedded in the binary by
[bpf2go](https://pkg.go.dev/github.com/cilium/ebpf/cmd/bpf2go), and both the
object and the Go bindings it generates are committed. That is what makes
`go build` work on a clone with no clang installed, and it is why CI
regenerates them and fails if the Go side has drifted from the C.

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
BTF             -r--r--r-- 1 root root 6841206 Aug 13 15:29 /sys/kernel/btf/vmlinux
cgroup fs       cgroup2fs
controllers     cpuset cpu io memory hugetlb pids rdma misc dmem
clang           Ubuntu clang version 18.1.3 (1ubuntu1)

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

### Blocked time and runqueue delay are different numbers

**Context.** A thread that is not running is doing one of two things: waiting
for something to happen, or waiting for a CPU after it already happened.
`offcputime` reports both as one quantity, because from off the CPU they look
identical.

**Decision.** Measure them separately, from three scheduler tracepoints:

```
blocked         sched_switch(leaving, not runnable)  ..  sched_wakeup
runqueue delay  sched_wakeup                         ..  sched_switch(arriving)
```

A thread leaving the CPU **while still runnable** was preempted and goes
straight back to the runqueue, so there is no wakeup to wait for and the
runqueue clock starts at the switch. The tracepoint reports that case as
`TASK_RUNNING`, or as `TASK_REPORT_MAX` when the switch was a preemption —
and missing the second is the most expensive mistake available here.
Preemption is the *common* case on a busy machine, so filing it as blocked
would move the bulk of runqueue delay into the blocked column and invert
every conclusion the tool exists to reach.

The two have opposite remedies. Blocked time says make the thing being waited
on faster. Runqueue delay says the machine does not have enough CPU — or, once
phase 2 finishes, that the cgroup was not allowed to use what there was, which
is a third answer again and the one no existing tool separates.

**Consequences.** The decomposition is per thread and it closes: on-CPU,
runqueue, blocked and unknown sum to the time each thread was observed, and
the tool prints whether they did rather than asserting it. Two things that are
deliberately visible in that sentence:

- **Observed, not the session window.** A thread first seen halfway through
  has half a window nobody watched. Reporting percentages of the session would
  silently attribute time that was never measured, so each thread reports the
  span it was actually watched for.
- **The residual is printed, and it found a real bug.** Across the whole
  machine it now reads `every decomposition sums to 100% of the time
  observed` — exactly zero, five runs out of five. It did not start that way.
  The first machine-wide run reported a decomposition off by 16 ms, then 60,
  then 132, while every synthetic thread closed perfectly.

  The cause was **thread 0**. Every CPU has its own idle task and all of them
  report pid 0, so one map entry was being written by sixteen cores at once;
  the accumulators are plain additions, correct for a thread that runs on one
  CPU at a time and not for sixteen concurrent writers. Skipping the idle task
  loses nothing — idle time belongs to a CPU, not to a thread — and the
  residual went to zero.

  A tool without that check would have printed the same plausible table and
  been quietly wrong. The residual is not decoration; it is the only thing
  that would have noticed. It is still printed when it is small, with a
  declared 1 ms tolerance for the cost of reading a map the kernel is writing
  to, and anything past that is called what it is.

### Aggregate in the kernel, stream only when you must

**Context.** A BPF program can keep a running total in a map that userspace
reads when it wants a number, or it can send every event up a ring buffer and
let userspace do the work. Phase 2 attaches to `sched_switch`, which fires
tens of thousands of times a second on a busy machine, so this choice stops
being stylistic.

**Options.** Both, implemented, and measured rather than argued about.

**Decision.** Aggregate in the kernel. Stream only where individual events
carry something a counter cannot hold.

The measurement is more lopsided than the reasoning predicted. Same machine,
same 10 second window, same tracepoint, nothing else changed:

| | delivered | lost |
|---|---|---|
| counting in a map | 13 455 entries, 72 processes | 0 |
| streaming every event | 1 591 513 events | **28 653 339** |

*WSL2, Debian 13, kernel 6.6.87.2-microsoft-standard-WSL2, 16 CPUs, 7.6 GB.*

The ring buffer delivered five per cent of what it was given. But the more
interesting number is the total: thirty million events in ten seconds on a
machine that, counted in the kernel, made thirteen thousand syscalls in the
same window. The observer produced the difference. Draining a ring buffer
costs syscalls, those syscalls hit the same tracepoint, and each one produces
another event to drain.

Filtering the stream to a process that is not the observer settles it:

```
$ sudo wallclock syscount -for 10s -stream -pid 539
2506 events
no events lost
```

Two and a half thousand events, nothing lost, from the same buffer that was
discarding ninety-five per cent a moment earlier. The flood was self-inflicted.

**Consequences.** The categories in phase 2 are counters and histograms, and
they live in kernel maps. Streaming survives for the cases where a single
event carries irreducible detail — a stack id, a destination address — and
those are sampled or filtered hard at the source rather than fanned out.

It also sets the standard for what "measured" means here. A tool that had not
counted its drops would have reported 1 591 513 events with total confidence
and been wrong by a factor of twenty, and nothing in the output would have
hinted at it. That is the failure this project exists to not have, and it took
until the first program to meet it.

### Ring buffer, not perf buffer

**Context.** Getting bytes from a BPF program to userspace has two mechanisms.
`BPF_MAP_TYPE_PERF_EVENT_ARRAY` is the older one, per-CPU; `BPF_MAP_TYPE_RINGBUF`
arrived in 5.8 and is shared across CPUs.

**Decision.** The ring buffer, which is also why the kernel floor for this
project is 5.8.

Three properties decide it. It is one buffer rather than one per CPU, so
memory is not multiplied by core count — on the 16-CPU machine above, a perf
buffer sized the same per CPU would reserve sixteen times the memory to hold
the same events. Events keep their order across CPUs, and phase 2 exists to
correlate a `sched_wakeup` on one CPU with the `sched_switch` that follows it
on another, which an unordered per-CPU transport makes into guesswork.
And `bpf_ringbuf_reserve` hands back the final memory *before* the event is
written, so a full buffer is discovered before anything is copied instead of
after — which is the difference between the drop counter above costing a
branch and costing a wasted copy.

**When the perf buffer still wins.** When the consumer is per-CPU too and
ordering is irrelevant — a sampling profiler pinning a reader to each core
avoids all cross-CPU contention on a single shared buffer. And on kernels
before 5.8, where it is the only thing there is.

### What the verifier actually refused

**Context.** The verifier, not the compiler, is what rejects BPF programs, and
it is the thing that will cost the most time on this project. Describing it
abstractly is easy and worth little.

**Decision.** Keep the rejections as fixtures with tests that assert them, so
the record cannot age into fiction.

The first one here was a map lookup dereferenced without a null check —
ordinary C, and unacceptable kernel code:

```c
__u64 *count = bpf_map_lookup_elem(&counts, &tgid);
*count += 1;                     /* no check */
```

```
load program: permission denied:
    R0 invalid mem access 'map_value_or_null'
    processed 8 insns (limit 1000000)
```

`map_value_or_null` is the verifier's name for what the helper returns: either
a pointer into the map or NULL, and the program may not touch it until the
branch that separates those has been taken. The fix is two lines. The errno is
a red herring — `EACCES` is what every rejected load returns, whatever the
reason, which is why the log matters and the error code does not. The fixture
lives in [`internal/bpfload/testdata`](internal/bpfload/testdata/unchecked_lookup.bpf.c)
and a test asserts the kernel still refuses it.

Two more the verifier insisted on, both in
[`bpf/syscount.bpf.c`](bpf/syscount.bpf.c):

- **Every lookup, even one that cannot fail.** An array map indexed by a
  constant the verifier can prove is in range still returns a maybe-null
  pointer, and it will not take the program's word that this call site is
  different.
- **Every filter is a constant, not a parameter.** The filters are
  `const volatile` globals patched into `.rodata` before the load: `const` so
  the verifier folds an unused filter's branch away entirely, `volatile` so
  clang does not decide the initialiser was the final answer and delete the
  read.

The limits worth knowing before phase 2 — a 512-byte stack, loops that must be
bounded or use `bpf_loop`, and an instruction ceiling of a million — have not
been hit yet. The programs here are tens of instructions. That will change.

### Pids from the kernel are not the pids in your /proc

**Context.** `bpf_get_current_pid_tgid` returns the pid as the **initial**
namespace numbers it, always, regardless of where the program reading it sits.
Development here happens inside a WSL distribution, which is a pid namespace
like any container.

**Decision.** Carry the process name from the kernel in the map value, and say
so plainly when the numbers will not match the reader's `/proc`.

This surfaced as every row of the first working output showing `(exited)`: the
tool was resolving names by reading `/proc/<pid>/comm` for pids that number
processes in a namespace it cannot see. The pids were fine. The lookup was
meaningless — and, worse than meaningless, since the two ranges overlap, a
lookup that hit would have named an unrelated process with total confidence.

The same trap sits under the `-pid` filter, where it is quieter: a pid copied
from `ps` inside a container matches nothing in the kernel, and the tool
reports a busy process as idle. So the tool checks whether it is in the
initial pid namespace — the kernel gives that one a fixed inode,
`PROC_PID_INIT_INO` — and says so when it is not.

**Consequences.** Carrying a 16-byte name in the map value costs one copy per
process rather than per event, since it is only written by the insert that
first sees a process. The name is therefore the one it had when first seen; a
process that `exec`s later keeps the old one, which is the right trade for a
counter and would be the wrong one for an audit log.

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

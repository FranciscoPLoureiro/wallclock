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

**The block above is the target, not a screenshot.** Everything in it except
the per-destination network breakdown is measured today: on-CPU, runqueue
delay, cgroup throttling, and blocked time split by what the thread was
waiting on. Attributing network time to individual destinations is phase 4.
The real output is at [what works now](#what-works-now).

The two lines that matter are *runqueue* and *throttled*, and they are the
reason the project exists rather than a nicety: they look identical from
outside and they are opposite problems.

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
| 2 | Off-CPU, runqueue delay, and cgroup throttling separated | ✅ done |
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

Two identical spinning processes on an idle sixteen-core machine. One of them
is in a cgroup with `cpu.max` set to 20 ms in every 100 ms; the other is not:

```
$ sudo wallclock profile -for 6s -comm wc- -top 4
     tid  observed        on-cpu     runqueue    throttled  blocked  unknown  command
  138977     5.95s    20.2% 1.2s  0.5% 28.6ms  79.3% 4.72s  0.0% 0s  0.0% 0s  wc-capped
  138979     5.98s  100.0% 5.98s   0.0% 152µs      0.0% 0s  0.0% 0s  0.0% 0s  wc-free

2 threads observed
every decomposition sums to 100% of the time observed
no threads lost
```

**That is the whole argument in one table.** Both threads never stop asking
for CPU. One gets 20.2% of one — the quota it was given, measured, not
assumed — and spends 79.3% of its life *ready and forbidden*. The other gets
100%.

An off-CPU profiler reports the capped thread as roughly 80% "not running"
and offers no way to tell that from a machine with no CPUs to spare.
`runqueue 0.5%` is the sentence that distinguishes them: **nothing was
contended**. The fix is one line of a compose file, and a tool that reported a
single not-running number would have sent somebody to buy hardware.

The kernel's own tally for that cgroup over the same run: `nr_throttled 70`,
`throttled_usec 5596241`. See [below](#throttling-measured-twice) for what
that number is doing there.

Wall clock split into where it actually went, without any cgroup involved.
Forty-eight spinning threads on sixteen CPUs, so each one can have a third of
a CPU and no more:

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

### A thread seen for the first time halfway through

**Context.** A profiler that starts while the machine is already running sees
threads in the middle of their lives. A thread first appears in a
`sched_switch` or a wakeup, and everything before that instant is unknown to
the tool — but not unknown in principle, since `/proc` has been keeping score
all along.

**Options.** Count the missing time as zero, seed each thread's state from
`/proc` at startup, or start each thread's accounting at first sight and say
so.

**Decision.** Start at first sight, and report the span each thread was
*observed* rather than the length of the session.

Counting the gap as zero is the tempting one and it is wrong in a way that
does not announce itself: percentages of the session window would come out
right for threads that were there from the start and quietly understate every
thread that arrived later, and nothing in the output would distinguish them.
Seeding from `/proc` is defensible and buys less than it looks: it can say a
thread was sleeping at t=0 but not what it had been doing, so it replaces an
honest gap with a state that is only half known.

The tail end is handled by the opposite rule. **Which event brings news of a
thread decides what may be done with it**, and this is the part that took four
attempts:

| Event | May create an entry | May revive an exited one |
|---|---|---|
| switch-out | yes | no |
| switch-in, wakeup | yes | no |
| `wakeup_new` | yes | yes |
| exit | plants a headstone | — |

A dying thread's last act is a context switch, moments after its exit event,
and a tid is handed to somebody new soon after that. Both arrive at the same
map entry and mean opposite things. `wakeup_new` is the only event that means
"this task did not exist until now", so it is the only one allowed to start
the books over.

**Consequences.** A thread that never schedules during the window does not
appear at all — a process blocked from before the session began and still
blocked at the end is invisible. That is the honest report of a tool that
watches the scheduler, and it is stated here rather than left to be
discovered.

### What happens when the maps fill up

**Context.** Every map has a `max_entries`. Threads, target sets, cgroups and
blocked reasons all grow with the machine, and a BPF map that fills does not
raise anything: the insert simply fails.

**Decision.** Every failure is counted, and every counter is printed in the
report, whether or not it fired.

A full map is the worst failure available to a measurement tool, because it
does not look like one. The numbers stay plausible, the percentages still add
to 100%, and the tool has silently stopped seeing part of the machine. The
counters are the difference between a report that is incomplete and a report
that is incomplete *and says so*:

```
LOST 50000 scheduler events: the threads map is at max_entries, so some
threads are missing from this report entirely rather than misfiled. That is
events, not threads -- one untracked thread contributes one per context switch
```

That message is the second version. The first said "LOST 50000 threads
entirely" on a machine with three hundred threads, because the counter counts
events and was labelled as though it counted threads. A number the reader can
see is impossible destroys the credibility of every line around it, which is
the opposite of what a drop counter is for.

**Consequences.** The bounds are 16384 threads, 16384 targets, 4096 cgroups,
8192 distinct stacks and 32768 thread-and-stack pairs. Exited threads are
drained on every read, and their blocked reasons with them, which is what
keeps a machine with heavy process churn from reaching those bounds at all —
a twelve second window on a build machine produced 660 entries where ninety
threads existed, before that draining was added.

### Throttling, measured twice

**Context.** Runqueue delay answers "this thread was ready and did not run"
and stops there. Two opposite situations produce it: the machine had no CPU
free, or the machine had CPU free and the thread's cgroup had spent its quota
for the period. Buy a bigger machine; raise a limit. No existing tool
separates them, which is the reason this project exists.

**Decision.** Hook `throttle_cfs_rq` and `unthrottle_cfs_rq`, keep a running
total of throttled time per cgroup, and have each thread record that total
when it joins the runqueue. When it finally runs, the difference is exactly
how much of its wait its own quota caused.

A running total rather than a list of intervals, because a total is enough
and is right in every case: an episode spanning the whole wait, one starting
in the middle, and any number beginning and ending inside it. "Was the cgroup
throttled when I looked" gets the last of those wrong.

kprobes rather than fentry. fentry is cheaper and both functions are static in
the kernel's BTF, which makes it the more fragile of the two — and throttling
fires a handful of times per 100 ms period against tens of thousands of
context switches a second, so the saving is unmeasurable.

**The validation is the point.** These numbers come from kprobes on the
scheduler; the kernel keeps its own tally in `cpu.stat`, by a route that
shares no code with them. A thread spinning in a cgroup capped at 20%:

| | |
|---|---|
| wallclock, for the thread | **1.607485793 s** throttled |
| `cpu.stat`, for the cgroup | **1.607395 s** (`throttled_usec 1607395`) |

Ninety-one microseconds apart in 1.6 seconds, and a ratio of 1.0001 over
three consecutive runs. A test asserts it on every run, because the value of
a second opinion is that it keeps being asked.

**If the kernel had no throttling hooks.** `throttle_cfs_rq` is static and
could stop being reachable; a kernel could be built without
`CONFIG_CFS_BANDWIDTH` at all. The fallback is `cpu.stat`, polled from
userspace: `nr_throttled` and `throttled_usec` are always there when the
controller is, and a poll every few milliseconds bounds the windows to the
poll interval. That is enough to say *whether* a cgroup was throttled during
a thread's wait and useless for saying *how much* of a two-millisecond wait it
covered — so the categories would survive and their precision would not. It
is a real answer and a worse one, which is why it is the fallback.

**Consequences.** A thread's cgroup is learned from
`bpf_get_current_cgroup_id`, which answers for the *running* task — so a
thread learns its own cgroup when it is scheduled out, not when it is woken,
where the running task is whoever did the waking. Until then its waits are
filed as ordinary runqueue delay. In practice that is one scheduling round,
and threads change cgroup about as often as they change process.

That id is also what `-cgroup` filters on, and it is the one filter that works
from anywhere: cgroup ids are global, and pids are not. Every namespace
problem in this project — a `-pid` that matches nothing, a `/proc` lookup that
names the wrong process, a test that cannot find its own subject — comes back
to that difference.

The cgroup map is bounded like every other, and when it fills, that cgroup's
throttling becomes invisible and its threads' waits go back to looking like
contention — the two categories this whole section exists to separate,
silently merged again. So it is counted and reported rather than absorbed.

### Why a thread stopped, and one number that does not add up

**Context.** "Blocked 22%" is half an answer. Waiting on a socket, waiting on
a mutex and waiting on a disk are one event to the scheduler and three
different problems to whoever has to fix one.

**Decision.** Capture the kernel stack with `bpf_get_stackid` at the moment
the thread stops, and classify it in userspace.

At that moment and no other: the thread is still on the CPU it is leaving and
the frames underneath it are the call that decided to stop. Only for a thread
that is actually stopping — a preempted thread's stack describes whatever it
was in the middle of, which is not a reason for anything.

Classification is in userspace because BPF cannot turn an address into a
symbol, and because the addresses are wanted whole in any case for the flame
graph. Stacks are scanned innermost first, past the `schedule` frames that sit
on top of every wait there is, until something meaningful appears:

```
$ sudo wallclock profile -for 5s -reasons
  149161  5.01s  0.0% 864µs  0.0% 530µs  0.0% 0s  100.0% 5.01s  wallclock
            futex    99.8% 5s
  142781  5.01s  0.0% 296µs  0.0% 699µs  0.0% 0s  100.0% 5.01s  kworker/15:3
            other    100.0% 5.01s
```

**epoll gets its own category** rather than being folded into network, and
that is phase 3 arriving early. A Go runtime parks goroutines in a netpoller
and the OS thread sits in `epoll_wait`; that thread is idle, not slow, and
calling it network would report a healthy server as spending its life waiting
on sockets.

**The bug this found, and how the wrong diagnosis nearly buried it.** For a
while the reasons for some threads added to *more* than the blocked time they
explain — up to 18% over, which cannot be true of the same intervals. It was
printed, labelled a known defect, and deliberately not clamped, because a
report that always adds up and is sometimes wrong is the failure this project
is arranged against.

The first explanation was that the two totals are read about a millisecond
apart. That was written down, and it was wrong — not because the mechanism was
wrong but because the bound was. **What lands in the gap between two reads is
not the length of the gap; it is the whole of whatever wait happened to close
during it.** A thread blocked for 900 ms whose wait ends in that millisecond
contributes 900 ms, twice: once as the open wait the totals measured, once as
the completed row the kernel filed a moment later.

Finding it took one measurement rather than more reasoning — print every row
for the worst thread — and the answer was immediate: one row was byte for byte
the open interval.

```
row 5.676ms      sleep   ..do_nanosleep
row 1.999982733s futex   ..futex_wait_queue
row 78.213µs     sleep   ..do_nanosleep     <- open=78.213µs, counted twice
```

The fix is an ordering: rows first, totals second, open waits added last. Read
that way the failure inverts and becomes honest — an interval that completes
between the two reads is in neither, so it goes unattributed and says so. A
test asserts on every run that no thread is ever over-attributed, because the
two calls look interchangeable and are not.

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

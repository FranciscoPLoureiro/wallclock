# Findings

What running this project somewhere it had never run produced: four defects
that had been present for its entire life, one of them reporting confidently
wrong numbers, plus the answer to a question its own documentation had left
open.

Written after the session that added the kernel matrices. It is deliberately
separate from the other two documents and does not repeat them:
[`DECISIONS.md`](DECISIONS.md) is why the system is built the way it is, and
[`COMPATIBILITY.md`](COMPATIBILITY.md) is what a host must provide. This is
what was *learned by being wrong*, and how strongly each conclusion is
supported.

None of these was found by reading code. All of them were found by running the
same test suite on a machine that had not run it before.

---

## The mechanism they all share

Worth stating first, because four unrelated-looking failures turn out to be one
idea.

**CO-RE relocates struct field offsets. That is all it does.**

The good half is genuinely good. A BPF program that reads
`task_struct->cgroups->dfl_cgrp` cannot hardcode a byte offset, because
`task_struct` is reshaped by every config change — measured here at 198 members
on 5.10 and 273 on Ubuntu 24.04. clang emits *relocations* instead of offsets,
the kernel publishes its own layout as BTF, and the loader patches each
instruction at load time. One object file, thirteen kernels, no recompilation.

The half that is easy to forget is that this covers **struct fields and
nothing else**. Four things a BPF program depends on are outside it:

| Dependency | Why CO-RE cannot help | What happened here |
|---|---|---|
| A **formatted tracepoint's** record layout | it is not a struct in BTF; reading `prev_pid` at byte 24 is a raw offset | wrong numbers on every Red Hat kernel |
| A **kprobe's symbol** existing | you cannot relocate a function that was never compiled | the tool refused to start on kernels without CFS bandwidth |
| A **stack frame's name** | the compiler and the unwinder decide | a test failed on a kernel where the answer was correct |
| The **userland** around it | not the kernel's business at all | the flagship test skipped silently off Debian |

Every one of those looks like a different bug. None of them looks like a CO-RE
failure, which is exactly why a matrix that only varies the kernel *version*
would have missed three of the four.

---

## 1. Wrong numbers, silently, on every Red Hat kernel

**What it looked like.** On Rocky 9, `preflight` passed every requirement, and
then:

```
      tid  observed      on-cpu   runqueue  throttled     blocked  command
  6911073        3s      0.0% 0s  0.0% 75µs   0.0% 0s   100.0% 3s
      103     2.97s      0.0% 0s 0.0% 262µs   0.0% 0s 100.0% 2.97s
  7102830     2.65s      0.0% 0s  0.0% 59µs   0.0% 0s 100.0% 2.65s

31 threads observed (8 shown)
every decomposition sums to 100% of the time observed
no threads lost
```

`tid 6911073` is not a process id. The command column is empty or garbage. And
underneath it, **both of the integrity guarantees this tool advertises are
printed, and both are true** — of threads that do not exist.

**How it was found.** The `syscount` suite failed differently: zero events. But
running `wallclock syscount` *unfiltered* on the same host worked and listed
real processes, while filtering to a specific syscall number returned nothing
even as a program made five hundred such calls. The event was arriving; the
number read out of it was wrong. That points at offsets, not at attachment.

**The cause.**

```
$ cat /sys/kernel/tracing/events/sched/sched_switch/format

                    Arch 7.1.9    Rocky 9 (5.14.0-687.el9_8)
  prev_comm              8            12
  prev_pid              24            28
  prev_state            32            40      ← eight, not four
  next_comm             40            48
  next_pid              56            64
```

Red Hat's 9.x kernel carries the PREEMPT_RT patchset's lazy preemption, which
adds `preempt_lazy_count` to `struct trace_entry` — the common header that
**every** tracepoint record begins with. Everything after it moves, and not by
a constant: four bytes for the four-byte fields, eight for `prev_state`,
because it is eight bytes and must be aligned. A single shift would have fixed
one field and broken the other.

The programs declared the context as a struct and read fields at compile-time
offsets, so `prev_pid` came from the last four bytes of `prev_comm`.

**Why nothing caught it.** This project had already met this hazard once, on
`sched_process_fork`, and the comment recording it states the rule correctly:
*tracepoint field names are stable ABI and their offsets are not.* Those
programs were rewritten as raw tracepoints because of it.

But it was learned in the form that announces itself. There, a field became a
dynamic string and the offsets after it moved *past the end* of the record —
and the kernel refuses to attach a program that reads beyond a tracepoint's
size, with `EACCES`.

This is the other form. The record **grew**. Every read stayed inside it.

> A read that goes out of bounds is caught by the kernel. A read that lands on
> the wrong field of the right record is caught by nothing.

**The fix.** [`internal/tracefs`](../internal/tracefs) reads the layout from
the kernel that is about to load the programs and writes the real offsets into
`const volatile` globals before load. The programs read through
`bpf_probe_read_kernel`, because the verifier permits context reads at constant
offsets only and refuses a pointer formed by adding a runtime value to the
context.

A field that is missing, or that kept its name and changed width, is an error
naming the field and both offsets — **not** a fallback to the compiled-in
value, because falling back is precisely the behaviour that produced the wrong
numbers.

**The attribution was wrong the first time, and that is worth recording.** The
first write-up of this said the field comes from mainline's `PREEMPT_LAZY`,
upstream in 6.13. Checked against three kernels:

| kernel | config | field present |
|---|---|---|
| Arch 7.1.9 | `ARCH_HAS_PREEMPT_LAZY=y`, `PREEMPT_LAZY` unset | no |
| ci-kernels 7.1.1 | `ARCH_HAS_PREEMPT_LAZY=y`, **`PREEMPT_LAZY=y`** | **no** |
| Rocky 9 (5.14) | **`HAVE_PREEMPT_LAZY=y`** | **yes** |

The middle row refutes it. Mainline's `PREEMPT_LAZY` is enabled there and adds
nothing. Two different implementations sharing most of a name — and the config
symbol is the only thing that tells them apart.

The fix never depended on the answer, which is the argument for reading the
layout rather than reasoning about versions and config symbols in the first
place.

---

## 2. A kernel that cannot throttle could not run the tool at all

Every `offcpu` test on the BPF CI images failed on `attach a kprobe to
throttle_cfs_rq`. The first hypothesis — the symbol was inlined away — was
wrong: it is not in `/proc/kallsyms` at all, because the guest's config says
`# CONFIG_CFS_BANDWIDTH is not set`. There is no `cpu.max`, and `cpu.stat` has
no `throttled_usec`.

**The consequence was disproportionate.** `Open()` returned the attach error,
so `wallclock profile` did not start at all — no on-CPU, no runqueue, no
blocked, no network — for a category that *cannot occur on that host*. And
`preflight` reported all requirements met; the failure arrived later as a raw
kprobe error.

**What makes degrading safe, and where it is not.** Where the kernel cannot
throttle, throttled time is not *unknown*, it is **necessarily zero**. Running
without those probes cannot misfile anything, because there is nothing to
misfile. Where the kernel *does* account CFS bandwidth and the probe still will
not attach, the tool refuses — there the time is real, and carrying on would
file it as ordinary runqueue delay, which is the one confusion this tool exists
to remove.

The signal is read from `cpu.stat` rather than from `kallsyms` or a kernel
config, because it is the interface the kernel actually exposes,
`/proc/config.gz` is absent on plenty of hosts, and a missing symbol cannot
tell a feature that was compiled out apart from one the compiler inlined —
which is the whole distinction the decision rests on.

---

## 3. The test suite only ran fully on Debian

Three places copied `/bin/dash` to build a CPU-burning subject with a
recognisable `comm`. Debian and Ubuntu ship that file; Arch, Fedora and Red Hat
do not.

| Site | Behaviour without it |
|---|---|
| the throttling test | **`t.Skip`** — silent |
| `wallclock validate` | hard error, refuses to start |
| `scripts/compare-tools.sh` | `cp` fails |

On any non-Debian host the throttling test — the single claim this project
exists to make — vanished, and the suite reported green.

Resolved from a candidate list ending at `/bin/sh`, which POSIX guarantees, and
a host without one is now a **failure rather than a skip**: a machine with no
`/bin/sh` is broken rather than different, and the skip is how this stayed
invisible.

---

## 4. A test that asserted on a symbol rather than on behaviour

`TestABlockedSocketReadIsClassifiedAsNetwork` failed on 7.1 and passed on 5.10
through 6.10, deterministically. The classification was *correct* the whole
time — `network 100%` of the wait. The test required a stack frame literally
named `ping_recvmsg` or `inet_recvmsg`:

```
6.10:   sock_recvmsg;inet_recvmsg;raw_recvmsg;skb_recv_datagram;…
7.1.1:  sock_recvmsg;             raw_recvmsg;skb_recv_datagram;…
```

**Stated honestly:** the frame is on the captured stack on 6.10 and not on 7.1,
same `ping` binary, same guest. The symbol is in `/proc/kallsyms` on both, so
it was *not* compiled out of existence — which was the first explanation given
here and is not supported. Whether it is inlining at that call site, a changed
call path, or the unwinder, is not established.

It does not need to be. *Which* of the two names appears at all already depends
on whether `ping` got a raw socket or an ICMP datagram socket, which depends on
whether it ran as root. The test was asserting on something the kernel never
promised. It now matches the **tid of the `ping` it started itself** — stricter
about whose wait it is looking at, and indifferent to all of that.

---

## 5. `destinations` cannot see Redis, and does not say so

Across eighteen runs of a service where **every** request goes through Redis —
the stock script, the idempotency keys and the rate limiter are all Redis —
port 6379 appears zero times. PostgreSQL and RabbitMQ appear in all eighteen.
Reproduced directly afterwards: in the same fifteen-second window PostgreSQL
shows 28 001 exchanges and Redis shows none.

`netlat` pairs a `tcp_sendmsg` with the next `tcp_recvmsg` on the same
connection, which is what a request/response protocol does. A client that
pipelines cannot be paired.

**The defect is not that.** The comment above the program said such a
connection *"is measured as something else, and the report says so rather than
pretending otherwise"*. The report does not say so. The connection is simply
absent — and an absence caused by not measuring reads exactly like an absence
of traffic. A reader of that table concludes the service has two dependencies.
It has three.

Not fixed. The comment now describes what actually happens, and this is the
first thing to fix the day anyone runs this tool for real rather than reads
about it.

---

## 6. Why the QEMU matrix never worked

`COMPATIBILITY.md` used to carry a write-up of seven CI rounds that failed,
ending:

> *What is not established is why it still hangs. Candidates not ruled out: the
> runner's KVM support being unusable in a way that makes the guest too slow
> rather than broken; vimto's result channel; something about how the binary is
> shared into the guest.*

It is established now, and it is none of the first two.

**Two wrong turns first, because they are instructive.** The leading hypothesis
was that GitHub's runners carry `/dev/kvm` without making it readable by the
runner user, so QEMU falls back to software emulation and a guest thirty times
slower is indistinguishable from a hung one. Plausible, documented workaround,
fits the symptom. Wrong: with the udev rule applied the runner reports
`/dev/kvm` at 0666, four CPUs exposing `vmx`/`svm`, QEMU initialising the
accelerator — and `vimto exec /bin/true` returning in **two seconds**.

The second was `-cpu max`, which vimto hardcodes and its config cannot
override, and which is a known cause of hangs under nested virtualisation. Also
plausible. Never tested, because bisecting made it unnecessary.

**The bisect**, one factor at a time on the runner:

```
plain (exec /bin/true)              ok
as root (-sudo)                     ok
4 cpus                              ok
2G of memory                        ok
4 cpus and 2G                       ok
/bin/sh -c 'echo hello'             timed out
```

Not root, not CPUs, not memory. The failing case differs in two ways — it is a
shell, and it writes output — so:

```
a binary that writes nothing        ok
a shell that writes nothing         ok        ← not the shell
a binary that writes one line       timed out
a shell that writes one line        timed out
a shell that writes to stderr       timed out
a failing command, no output        returned 1, in under two seconds
```

**The finding.** A vimto guest reaches its command, runs it, and returns its
exit status correctly. It then blocks forever on the first byte that command
writes to stdout or stderr. Exit statuses cross the boundary in both
directions. The kernel's own serial console is a separate channel and works,
which is why the guest boots and says so while the command inside it cannot say
anything.

**Why this explains the original seven rounds.** That write-up records the tool
*"never returning"* under both `preflight` and `go test` — both of which write
on their first line — and one round that *"got as far as executing a setup
command and failing inside it"*, which is a failure with **no output**, exactly
the case that returns. It also notes the hang was unchanged with a static
binary and with no guest setup, which is consistent, because none of that
touches the output channel.

From inside a hang there is no way to distinguish "never started" from "started
and cannot speak".

**The workaround.** The repository is already mounted read-write into the guest
at the same path it has on the host, so the guest writes its log there and the
driver reads the file after vimto exits. The broken channel is never used.

Five kernels now run in about ninety seconds on every pull request.

---

## How strongly each of these is supported

Ranked by what the evidence actually carries, because three claims in this
document were stated confidently during the work and then turned out to be
wrong.

| Claim | Status | Basis |
|---|---|---|
| `/bin/dash` made the throttling test skip off Debian | **Certain** | file absent, test skipped, fix makes it run |
| The BPF CI images cannot throttle | **Certain** | config, absent symbol and `cpu.stat` all agree |
| RHEL tracepoint offsets differ and produced wrong output | **Certain** | two format files, garbage tids observed, fix → full pass |
| The extra field is RT-style lazy preemption, not mainline `PREEMPT_LAZY` | **Verified** | three kernels' configs; 7.1.1 has `PREEMPT_LAZY=y` and no field |
| A vimto guest blocks on its first byte of output | **Strong** | six-case bisect with a clean split; the fix works |
| …and that is what defeated the original seven rounds | **Inference** | fits every recorded symptom; that environment cannot be re-run |
| Redis is invisible to `destinations` | **Certain** | 0 of 18 runs, plus a direct reproduction |
| …because the client pipelines | **Plausible** | consistent with the documented pairing rule; not instrumented |
| `inet_recvmsg` was "inlined away" on 7.1 | **Retracted** | the symbol is in `kallsyms` on every kernel checked |
| The CI failures were KVM being inaccessible | **Retracted** | KVM works on the runner; `/bin/true` returns in 2 s |
| `-cpu max` under nested virtualisation was the cause | **Retracted** | never tested; the bisect made it unnecessary |

Three retractions. What caught all three was the same property: the matrix
records **`no result`** rather than a pass or a failure when a guest does not
finish, so an exit status could never stand in for evidence the run had
happened. That is the rule this project applies to its own checks, turned on
the harness that checks it.

**Still unknown:** which component blocks on write in a vimto guest; whether
other vendor kernels (SUSE, Amazon Linux, Oracle UEK) carry the same header
change; why the `inet_recvmsg` frame stopped appearing between 6.10 and 7.1;
and whether the Redis blindness is purely pipelining or also connection reuse.

---

## What is left

1. **Measure the overhead again.** Every tracepoint field read now goes through
   `bpf_probe_read_kernel`. The instrument exists — `make overhead` — and the
   before-figures are in [`DECISIONS.md`](DECISIONS.md#how-this-was-validated-and-what-it-costs).
   A profiler that got measurably more expensive and did not say so would be
   the same class of defect as everything above.
2. **Report the vimto behaviour upstream**, with the bisect table.
3. **Make `destinations` admit what it could not pair.** Even without pairing
   pipelined traffic, listing the connections it saw and could not measure
   turns a silent omission into a stated one.
4. **arm64.** Two of the four programs compile for it; the two using kprobes
   need arm64's `pt_regs` layout, which an x86-64 host does not have. Compiling
   is a long way short of loading.
5. **More vendor kernels**, now that adding a row to the distro matrix is a
   one-line change.

Not worth doing: more mainline kernels. The span is already 4.19 to 7.1, and
the interesting variation was never between mainline versions — it was between
*builds* of the same one.

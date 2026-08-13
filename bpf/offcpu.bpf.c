// SPDX-License-Identifier: GPL-2.0
/*
 * Where a thread's wall clock actually goes, from the scheduler's own events.
 *
 * A thread is always in one of four states: running on a CPU, ready and
 * waiting for one, ready and held back by its cgroup's quota, or blocked
 * waiting for something else. A CPU profiler sees the first and is blind to
 * the other three, which is where the time goes when a service gets slow.
 *
 * This file measures the first, second and fourth. Throttling -- the third,
 * and the one no existing tool separates -- is the next piece of work, and
 * the state machine here is shaped to have it inserted rather than bolted on:
 * runqueue time is already accumulated separately from blocked time, and
 * splitting it by whether it fell inside a throttled window is an addition to
 * the report rather than a change to the accounting.
 *
 * The two quantities that are easy to confuse, and the reason this is not
 * just offcputime:
 *
 *   blocked        sched_switch(leaving, not runnable) .. sched_wakeup
 *   runqueue delay sched_wakeup .. sched_switch(arriving)
 *
 * The first is waiting for something to happen. The second is waiting for a
 * CPU after it already happened. They have opposite fixes -- make the thing
 * faster, versus give the machine more CPU -- and off-CPU profilers report
 * them as one number.
 */
#include <linux/bpf.h>
#include <bpf/bpf_helpers.h>
#include <bpf/bpf_core_read.h>
#include <bpf/bpf_tracing.h>

char LICENSE[] SEC("license") = "Dual BSD/GPL";

/*
 * Enough of task_struct to read a thread id, and not one field more.
 *
 * preserve_access_index is what makes this work on a kernel it has never
 * seen: clang records "the field named pid inside task_struct" instead of a
 * byte offset, and the loader resolves that against the target kernel's own
 * BTF before the verifier ever sees the program. Declaring two fields of a
 * struct that has hundreds is not a shortcut -- CO-RE matches by name, so
 * what is left out cannot be got wrong.
 */
struct task_struct {
	int pid;
} __attribute__((preserve_access_index));

/*
 * Tracepoint contexts, declared from the format files rather than from
 * vmlinux.h. A tracepoint's layout is stable ABI -- userspace reads it out of
 * the format file under /sys/kernel/tracing/events/sched, and these were
 * copied from there -- so no CO-RE relocation is needed. That stops being
 * true the moment this reads a field of task_struct, which is what the
 * throttling work will need.
 */
struct sched_switch_ctx {
	__u64 __common;
	char prev_comm[16];
	__s32 prev_pid;
	__s32 prev_prio;
	__s64 prev_state;
	char next_comm[16];
	__s32 next_pid;
	__s32 next_prio;
};

struct sched_wakeup_ctx {
	__u64 __common;
	char comm[16];
	__s32 pid;
	__s32 prio;
	__s32 target_cpu;
};

/*
 * sched_process_fork has no context struct here on purpose, and the reason is
 * the most useful thing this file has to teach.
 *
 * It had one, copied from the format file on the development kernel:
 *
 *     field:char parent_comm[16];  offset:8;   size:16;      (6.6)
 *     field:pid_t parent_pid;      offset:24;  size:4;
 *     field:char child_comm[16];   offset:28;  size:16;
 *     field:pid_t child_pid;       offset:44;  size:4;
 *
 * On 6.17 the same tracepoint reports:
 *
 *     field:__data_loc char[] parent_comm;  offset:8;  size:4;
 *
 * The names moved to dynamic strings, every offset after them shifted, and a
 * read of child_pid at 44 lands past the end of the record. The kernel
 * catches that -- it refuses to attach a program whose highest context read
 * exceeds the tracepoint's size -- and refuses with EACCES, which arrives as
 * "permission denied" and looks like a capability problem. It cost a day of
 * looking in the wrong place, and it happened only on CI, because CI runs the
 * newer kernel.
 *
 * So the comment above about tracepoint layouts being stable ABI is true of
 * the field *names* and not of their offsets. The raw tracepoint below is
 * given the task_struct pointers the kernel passes internally, and CO-RE
 * relocates the field read against whatever kernel is loading it -- which is
 * the mechanism this project is built on, arriving where it was needed rather
 * than where it was planned.
 */

/*
 * What the scheduler reports for a thread leaving the CPU.
 *
 * Zero is TASK_RUNNING: it left while still runnable, so it went back to the
 * runqueue. TASK_REPORT_MAX is what the tracepoint reports when the switch
 * was a preemption, which also means still runnable -- and missing that case
 * would file every preempted thread as blocked, which is the single most
 * expensive mistake available here: preemption is the common case on a busy
 * machine, and it would move the bulk of runqueue delay into the blocked
 * column and invert the conclusion.
 */
#define TASK_RUNNING 0x0000
#define TASK_REPORT_MAX 0x0100

enum thread_state {
	STATE_UNKNOWN = 0,
	STATE_ON_CPU = 1,
	STATE_RUNQUEUE = 2,
	STATE_BLOCKED = 3,
};

struct thread {
	/* When this thread was first seen. Everything below is measured from
	 * here, not from when the session started: a thread that only appears
	 * halfway through has half a window of history nobody observed, and
	 * pretending otherwise would put that time in a category. */
	__u64 first_seen_ns;
	/* When the current state began. */
	__u64 since_ns;
	__u64 on_cpu_ns;
	__u64 runqueue_ns;
	__u64 blocked_ns;
	__u32 state;
	__u32 tid;
	char comm[16];
};

#define MAX_TRACKED_THREADS 16384

struct {
	__uint(type, BPF_MAP_TYPE_HASH);
	__uint(max_entries, MAX_TRACKED_THREADS);
	__type(key, __u32); /* tid, which the kernel calls a pid */
	__type(value, struct thread);
} threads SEC(".maps");

/*
 * Which threads to follow. Empty means all of them.
 *
 * Userspace fills this from /proc/<pid>/task when a target is named, and the
 * fork program below adds children of tracked threads so that a thread
 * created after the session started is not silently missed -- which would
 * show up as a process whose accounted time shrinks as it scales up.
 */
struct {
	__uint(type, BPF_MAP_TYPE_HASH);
	__uint(max_entries, MAX_TRACKED_THREADS);
	__type(key, __u32);
	__type(value, __u8);
} targets SEC(".maps");

const volatile __u8 filter_targets = 0;

enum stat_slot {
	STAT_THREADS_FULL = 0,
	STAT_TARGETS_FULL = 1,
	STAT__MAX = 2,
};

const enum stat_slot *unused_stat_slot __attribute__((unused));
const struct thread *unused_thread __attribute__((unused));

struct {
	__uint(type, BPF_MAP_TYPE_ARRAY);
	__uint(max_entries, STAT__MAX);
	__type(key, __u32);
	__type(value, __u64);
} stats SEC(".maps");

static __always_inline void bump(__u32 slot)
{
	__u64 *counter = bpf_map_lookup_elem(&stats, &slot);
	if (counter)
		__sync_fetch_and_add(counter, 1);
}

static __always_inline int tracked(__u32 tid)
{
	/*
	 * Thread 0 is the idle task, and it is not one thread.
	 *
	 * Every CPU has its own swapper and all of them report pid 0, so a
	 * single map entry ends up written by every core at once. The
	 * accumulators here are plain additions -- correct for a thread, which
	 * runs on one CPU at a time -- and under sixteen concurrent writers
	 * they lose updates and interleave timestamps. The symptom was a
	 * decomposition that missed closing by tens of milliseconds while every
	 * real thread closed exactly, which is how this was found: the residual
	 * is not decoration.
	 *
	 * Skipping it loses nothing. Idle time belongs to the CPU, not to a
	 * thread, and no profiler reports the swapper.
	 */
	if (tid == 0)
		return 0;
	if (!filter_targets)
		return 1;
	return bpf_map_lookup_elem(&targets, &tid) != 0;
}

/*
 * Move a thread into a new state, crediting the time it spent in the old one.
 *
 * This is the whole measurement. Everything else is plumbing that decides
 * which state a thread just moved into.
 */
static __always_inline void transition(__u32 tid, const char *comm,
				       enum thread_state next_state, __u64 now)
{
	struct thread *t = bpf_map_lookup_elem(&threads, &tid);
	if (!t) {
		/*
		 * First sight. Its history before this instant is not
		 * unknowable in principle -- /proc knows -- but it is unknown
		 * to this program, and inventing a state for it would put
		 * fabricated time in a real column. Accounting starts now, and
		 * userspace reports how long each thread was actually
		 * observed so the reader can see it is not the full window.
		 */
		struct thread fresh = {
			.first_seen_ns = now,
			.since_ns = now,
			.state = next_state,
			.tid = tid,
		};
		if (comm)
			__builtin_memcpy(&fresh.comm, comm, sizeof(fresh.comm));
		if (bpf_map_update_elem(&threads, &tid, &fresh, BPF_NOEXIST) < 0)
			bump(STAT_THREADS_FULL);
		return;
	}

	/*
	 * Refreshed on every transition, not only on the first sight of a
	 * thread. Threads are renamed and, more importantly, reused: a Go
	 * runtime hands a parked thread to the next goroutine that needs one,
	 * and a thread pool does the same. Keeping the first name seen means
	 * reporting a thread's time under the name of whatever ran on it
	 * before -- which showed up here as a test that could not find its own
	 * subject, and would show up in production as time attributed to the
	 * wrong worker.
	 *
	 * The cost is two eight-byte stores beside a map lookup that already
	 * dominates this function. The syscount program makes the opposite
	 * choice for the opposite reason: its key is a process, whose name
	 * rarely changes, and it is written once per process rather than once
	 * per context switch.
	 */
	if (comm)
		__builtin_memcpy(&t->comm, comm, sizeof(t->comm));

	__u64 elapsed = now - t->since_ns;
	switch (t->state) {
	case STATE_ON_CPU:
		t->on_cpu_ns += elapsed;
		break;
	case STATE_RUNQUEUE:
		t->runqueue_ns += elapsed;
		break;
	case STATE_BLOCKED:
		t->blocked_ns += elapsed;
		break;
	default:
		/* STATE_UNKNOWN holds no time: nothing sets it after the
		 * entry is created. */
		break;
	}

	t->state = next_state;
	t->since_ns = now;
}

SEC("tracepoint/sched/sched_switch")
int on_sched_switch(struct sched_switch_ctx *ctx)
{
	__u64 now = bpf_ktime_get_ns();
	__u32 prev = (__u32)ctx->prev_pid;
	__u32 next = (__u32)ctx->next_pid;
	/*
	 * Via the stack, not straight from the context.
	 *
	 * Copying ctx->prev_comm into a map value directly is rejected with
	 * "dereference of modified ctx ptr R1 off=8 disallowed": the verifier
	 * permits reads of the context at constant offsets, and refuses a
	 * pointer formed by adding to it and then dereferenced. Landing the
	 * array on the stack first turns the copy into loads the verifier can
	 * account for, and costs 16 bytes of the 512 available.
	 */
	char comm[16];

	if (tracked(prev)) {
		/*
		 * Still runnable means it was pushed off, not stopped: it goes
		 * straight back to the runqueue and the clock on runqueue
		 * delay starts here rather than at a wakeup that will never
		 * come.
		 */
		int runnable = ctx->prev_state == TASK_RUNNING ||
			       (ctx->prev_state & TASK_REPORT_MAX);
		__builtin_memcpy(comm, ctx->prev_comm, sizeof(comm));
		transition(prev, comm, runnable ? STATE_RUNQUEUE : STATE_BLOCKED, now);
	}

	if (tracked(next)) {
		__builtin_memcpy(comm, ctx->next_comm, sizeof(comm));
		transition(next, comm, STATE_ON_CPU, now);
	}

	return 0;
}

/*
 * A wakeup ends blocked time and begins runqueue delay. The thread is now
 * ready; whether it gets a CPU is somebody else's decision, and the gap until
 * it does is precisely what this project exists to measure.
 */
SEC("tracepoint/sched/sched_wakeup")
int on_sched_wakeup(struct sched_wakeup_ctx *ctx)
{
	__u32 tid = (__u32)ctx->pid;
	if (!tracked(tid))
		return 0;
	char comm[16];
	__builtin_memcpy(comm, ctx->comm, sizeof(comm));
	transition(tid, comm, STATE_RUNQUEUE, bpf_ktime_get_ns());
	return 0;
}

SEC("tracepoint/sched/sched_wakeup_new")
int on_sched_wakeup_new(struct sched_wakeup_ctx *ctx)
{
	__u32 tid = (__u32)ctx->pid;
	if (!tracked(tid))
		return 0;
	char comm[16];
	__builtin_memcpy(comm, ctx->comm, sizeof(comm));
	transition(tid, comm, STATE_RUNQUEUE, bpf_ktime_get_ns());
	return 0;
}

/*
 * Follow the children of tracked threads.
 *
 * Without this, a process that starts threads after the session began is
 * measured only through the threads it already had -- and the more work it
 * spreads out, the less of it the tool sees. That is the shape of error that
 * looks like an improvement.
 */
SEC("raw_tp/sched_process_fork")
int BPF_PROG(on_sched_process_fork, struct task_struct *parent_task,
	     struct task_struct *child_task)
{
	if (!filter_targets)
		return 0;

	__u32 parent = (__u32)BPF_CORE_READ(parent_task, pid);
	if (!bpf_map_lookup_elem(&targets, &parent))
		return 0;

	__u32 child = (__u32)BPF_CORE_READ(child_task, pid);
	__u8 yes = 1;
	if (bpf_map_update_elem(&targets, &child, &yes, BPF_ANY) < 0)
		bump(STAT_TARGETS_FULL);
	return 0;
}

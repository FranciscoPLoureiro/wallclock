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
#include <linux/errno.h>
/* struct pt_regs for the kprobe macros. bpf_tracing.h uses the kernel field
 * names when vmlinux.h is in play and the userspace ones otherwise, and on
 * x86-64 both describe the same layout, so the offsets come out right. */
#include <linux/ptrace.h>
#include <bpf/bpf_helpers.h>
#include <bpf/bpf_core_read.h>
#include <bpf/bpf_tracing.h>

char LICENSE[] SEC("license") = "Dual BSD/GPL";

/*
 * Enough of task_struct to tell a thread from a process, and not one field
 * more.
 *
 * preserve_access_index is what makes this work on a kernel it has never
 * seen: clang records "the field named pid inside task_struct" instead of a
 * byte offset, and the loader resolves that against the target kernel's own
 * BTF before the verifier ever sees the program. Declaring two fields of a
 * struct that has hundreds is not a shortcut -- CO-RE matches by name, so
 * what is left out cannot be got wrong.
 *
 * pid is the thread and tgid is the process, which is the opposite of what
 * userspace means by those words.
 */
struct task_struct {
	int pid;
	int tgid;
	char comm[16];
} __attribute__((preserve_access_index));

/*
 * The path from a throttled runqueue to the cgroup that owns it.
 *
 * Five structs, one field each, out of types that have hundreds between them.
 * The alternative is vmlinux.h, which for this kernel is 3.2 MB and 158,000
 * lines of generated header to reach five members -- and which would hide the
 * one thing worth seeing here, which is that the walk is by name and the
 * loader resolves every step of it against the kernel actually running.
 *
 * cfs_rq -> tg -> css.cgroup -> kn -> id is the same identity that
 * bpf_get_current_cgroup_id returns, arrived at from the scheduler's side
 * rather than from a task's, which is what lets a throttling event and a
 * waiting thread be matched to each other.
 */
struct kernfs_node {
	__u64 id;
} __attribute__((preserve_access_index));

struct cgroup {
	struct kernfs_node *kn;
} __attribute__((preserve_access_index));

struct cgroup_subsys_state {
	struct cgroup *cgroup;
} __attribute__((preserve_access_index));

struct task_group {
	struct cgroup_subsys_state css;
} __attribute__((preserve_access_index));

struct cfs_rq {
	struct task_group *tg;
} __attribute__((preserve_access_index));

/*
 * Tracepoint contexts, declared from the format files rather than from
 * vmlinux.h. Reading a formatted tracepoint at constant offsets is cheaper
 * than a CO-RE read, and for these three the layout was checked against both
 * kernels this project runs on.
 *
 * That check is not a formality -- see the note on sched_process_fork below,
 * where it was not done and cost a day.
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
 * sched_process_fork and sched_process_exit have no context struct here on
 * purpose, and the reason is the most useful thing this file has to teach.
 *
 * fork had one, copied from the format file on the development kernel:
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
 * So tracepoint field *names* are stable ABI and their offsets are not. The
 * raw tracepoints below are given the task_struct pointers the kernel passes
 * internally, and CO-RE relocates the field reads against whatever kernel is
 * loading them. Raw tracepoints are also immune to arguments being appended,
 * which both of these have had across versions.
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
 *
 * The two cannot collide. __trace_sched_switch_state returns TASK_REPORT_MAX
 * on its own for a preemption, and otherwise 1 << (index - 1) where the
 * largest reportable index yields 0x80.
 */
#define TASK_RUNNING 0x0000
#define TASK_REPORT_MAX 0x0100

enum thread_state {
	STATE_UNKNOWN = 0,
	STATE_ON_CPU = 1,
	STATE_RUNQUEUE = 2,
	STATE_BLOCKED = 3,
	/*
	 * The thread is gone and its numbers are final. Nothing accumulates
	 * into an exited entry, and userspace drains it on the next read.
	 *
	 * Without this state a dead thread stays parked in whatever it was
	 * doing last -- which at exit is a switch out in a non-runnable state,
	 * so BLOCKED -- and its blocked time grows for as long as the session
	 * runs. Measured: /bin/true, alive for about a millisecond, reported
	 * as blocked for 12.02 seconds of a 12 second window, and 660 threads
	 * "observed" on a machine with ninety.
	 */
	STATE_EXITED = 4,
};

struct thread {
	/* When this thread was first seen. Everything below is measured from
	 * here, not from when the session started: a thread that only appears
	 * halfway through has half a window of history nobody observed, and
	 * pretending otherwise would put that time in a category. */
	__u64 first_seen_ns;
	/* When the current state began. For an exited thread this is when it
	 * exited, which is what makes its observation window closed. */
	__u64 since_ns;
	__u64 on_cpu_ns;
	__u64 runqueue_ns;
	__u64 blocked_ns;
	/*
	 * The half of runqueue delay that the cgroup's own quota caused.
	 *
	 * Both halves are a thread that is ready and not running, and from
	 * outside they are the same thing. They are opposite problems: one says
	 * the machine has no CPU to give, the other says the machine had CPU
	 * and the cgroup was not allowed to use it. A bigger machine fixes the
	 * first and does nothing for the second; one line of a compose file
	 * fixes the second and nothing for the first. Reporting them as one
	 * number is how a team buys hardware it did not need.
	 */
	__u64 throttled_ns;
	/* Time credited to no named state. It should always be zero, and it is
	 * accumulated rather than merely displayed so that the day a state is
	 * added without a case in credit_elapsed, the report says so instead of
	 * quietly losing the time. */
	__u64 unknown_ns;
	/*
	 * The cgroup this thread was last seen in, and the cumulative throttled
	 * time of that cgroup at the instant the thread joined the runqueue.
	 * The difference between that snapshot and the same total when the
	 * thread finally runs is exactly how much of its wait the quota caused.
	 */
	__u64 cgroup_id;
	__u64 throttle_snapshot_ns;
	/* The kernel stack the thread stopped on, or negative if it could not
	 * be captured. Valid only while the thread is blocked. */
	__s32 block_stack_id;
	__u32 state;
	__u32 tid;
	/*
	 * The process this thread belongs to, or zero until something that
	 * knows has said so.
	 *
	 * A thread is the unit the scheduler moves and the wrong unit to reason
	 * about a service with. A Go runtime spreads one server across as many
	 * threads as it has cores and hands them to whichever goroutine needs
	 * one next, so no single thread's decomposition describes the process
	 * and reading them one by one describes nothing at all. Rolling them up
	 * needs the group they belong to, and this is it.
	 *
	 * Sticky, and filled in when the answer arrives rather than demanded up
	 * front, for the same reason cgroup_id is: only some of these events
	 * can answer the question. At sched_switch the running task is the one
	 * leaving, so bpf_get_current_pid_tgid speaks for prev and says nothing
	 * about next; at a wakeup it speaks for whoever did the waking. The
	 * exit program has the task_struct in its hand and is authoritative,
	 * which is what makes even a thread that never switched out end up with
	 * a process.
	 *
	 * It costs nothing: the four bytes land in the padding the compiler was
	 * already adding after comm[16].
	 */
	__u32 tgid;
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
 * fork program below adds threads the target starts afterwards, so that a
 * process which grows its pool mid-session is not measured only through the
 * threads it began with -- an error that looks like the process doing less
 * work as it scales up.
 */
struct {
	__uint(type, BPF_MAP_TYPE_HASH);
	__uint(max_entries, MAX_TRACKED_THREADS);
	__type(key, __u32);
	__type(value, __u8);
} targets SEC(".maps");

const volatile __u8 filter_targets = 0;

/*
 * How long each cgroup has spent throttled, as a running total plus whichever
 * episode is open right now.
 *
 * A total rather than a list of intervals, because a total is enough: a
 * thread that joins the runqueue records the total it sees, and when it
 * finally runs, the difference is how much of its wait was quota. That works
 * for an episode that spans the whole wait, one that starts in the middle,
 * and any number that begin and end inside it -- none of which a "was it
 * throttled when I looked" test gets right.
 */
struct throttle {
	/* Completed episodes. */
	__u64 total_ns;
	/* When the open episode began, or zero if the cgroup is running. */
	__u64 since_ns;
	__u64 episodes;
};

#define MAX_TRACKED_CGROUPS 4096

struct {
	__uint(type, BPF_MAP_TYPE_HASH);
	__uint(max_entries, MAX_TRACKED_CGROUPS);
	__type(key, __u64); /* cgroup id, as bpf_get_current_cgroup_id returns it */
	__type(value, struct throttle);
} throttled SEC(".maps");

const struct throttle *unused_throttle __attribute__((unused));

/*
 * Why a thread stopped, taken from the kernel stack at the moment it stopped.
 *
 * The function on top says everything: tcp_recvmsg is a socket, futex_wait is
 * a lock, io_schedule is a disk, do_nanosleep is a deliberate pause. Those are
 * one event to the scheduler and four different problems to whoever has to
 * fix one of them.
 *
 * The stack is captured here and classified in userspace, because BPF cannot
 * turn an address into a symbol -- and because the addresses are wanted whole
 * in any case, to fold into a flame graph.
 */
#define MAX_STACK_DEPTH 32
#define MAX_STACKS 8192

struct {
	__uint(type, BPF_MAP_TYPE_STACK_TRACE);
	__uint(max_entries, MAX_STACKS);
	__uint(key_size, sizeof(__u32));
	__uint(value_size, MAX_STACK_DEPTH * sizeof(__u64));
} stacks SEC(".maps");

/*
 * Blocked time, split by the stack that caused it.
 *
 * Keyed by thread as well as by stack, so that the same wait -- every thread
 * in a pool parked on one futex -- can still be told apart by who was doing
 * the waiting. These totals add up to the thread's blocked_ns, and userspace
 * checks that they do rather than trusting that they must.
 */
struct blocked_key {
	__u32 tid;
	__s32 stack_id;
};

#define MAX_BLOCKED_REASONS 32768

struct {
	__uint(type, BPF_MAP_TYPE_HASH);
	__uint(max_entries, MAX_BLOCKED_REASONS);
	__type(key, struct blocked_key);
	__type(value, __u64);
} blocked_by SEC(".maps");

const struct blocked_key *unused_blocked_key __attribute__((unused));

/*
 * The cumulative time a cgroup has spent throttled as of now.
 *
 * Only meaningful evaluated at the present instant -- there is no way to ask
 * what this was a second ago, which is why threads take a snapshot when they
 * start waiting rather than reconstructing it afterwards.
 */
static __always_inline __u64 throttled_upto_now(__u64 cgroup_id, __u64 now)
{
	if (!cgroup_id)
		return 0;
	struct throttle *th = bpf_map_lookup_elem(&throttled, &cgroup_id);
	if (!th)
		return 0;
	__u64 total = th->total_ns;
	if (th->since_ns && now > th->since_ns)
		total += now - th->since_ns;
	return total;
}

enum stat_slot {
	/*
	 * Scheduler events, not threads. When the threads map is full the
	 * insert fails, the entry never exists, and the next event for the
	 * same thread takes the same path -- so this counts events dropped and
	 * is much larger than the number of threads behind them. Reporting it
	 * as a thread count printed "LOST 50000 threads entirely" on machines
	 * with a few hundred.
	 */
	STAT_EVENTS_DROPPED = 0,
	STAT_TARGETS_FULL = 1,
	STAT_CGROUPS_FULL = 2,
	/* A stack could not be captured or its total could not be recorded, so
	 * that blocked time has no reason attached to it and lands in the
	 * unattributed line of the report rather than vanishing. */
	STAT_STACKS_LOST = 3,
	STAT__MAX = 4,
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
	/*
	 * An array map lookup with an index the verifier can see is in range
	 * cannot fail, and the verifier still refuses the program without this
	 * check: it knows the helper returns a pointer that may be null and
	 * will not take anyone's word that this call site is different.
	 */
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

static __always_inline void start_fresh(struct thread *t, __u32 tid, __u32 tgid,
					const char *comm, enum thread_state state,
					__u64 now)
{
	t->first_seen_ns = now;
	t->since_ns = now;
	t->on_cpu_ns = 0;
	t->runqueue_ns = 0;
	t->blocked_ns = 0;
	t->throttled_ns = 0;
	t->unknown_ns = 0;
	t->cgroup_id = 0;
	t->throttle_snapshot_ns = 0;
	t->block_stack_id = -1;
	t->state = state;
	t->tid = tid;
	t->tgid = tgid;
	if (comm)
		__builtin_memcpy(&t->comm, comm, sizeof(t->comm));
}

static __always_inline void credit_elapsed(struct thread *t, __u64 now)
{
	__u64 elapsed = now - t->since_ns;
	switch (t->state) {
	case STATE_ON_CPU:
		t->on_cpu_ns += elapsed;
		break;
	case STATE_RUNQUEUE: {
		/*
		 * The whole point of the phase, in six lines. The thread was
		 * ready and not running; the question is whether nobody had a
		 * CPU to give it, or whether its cgroup was not allowed to use
		 * one that was free.
		 *
		 * The cgroup's cumulative throttled time has moved on by
		 * exactly the amount of this wait that the quota caused, since
		 * the snapshot was taken when the wait began.
		 */
		__u64 throttled_now = throttled_upto_now(t->cgroup_id, now);
		__u64 quota = 0;
		if (throttled_now > t->throttle_snapshot_ns)
			quota = throttled_now - t->throttle_snapshot_ns;
		/*
		 * Clamped, and it should never bind. It would take the thread
		 * changing cgroups mid-wait, or a snapshot that was never
		 * taken -- and the alternative to clamping is a category that
		 * exceeds the wall clock it is a share of, which would break
		 * the decomposition rather than merely be wrong.
		 */
		if (quota > elapsed)
			quota = elapsed;
		t->throttled_ns += quota;
		t->runqueue_ns += elapsed - quota;
		break;
	}
	case STATE_BLOCKED:
		t->blocked_ns += elapsed;
		/*
		 * The same interval, filed a second time against the stack the
		 * thread stopped on, so that "blocked" can become "blocked in
		 * tcp_recvmsg". The two totals are redundant on purpose:
		 * userspace adds these up per thread and checks the sum against
		 * blocked_ns, and a disagreement means reasons are being lost.
		 */
		if (t->block_stack_id >= 0) {
			struct blocked_key key = {
				.tid = t->tid,
				.stack_id = t->block_stack_id,
			};
			__u64 *reason = bpf_map_lookup_elem(&blocked_by, &key);
			if (reason) {
				*reason += elapsed;
			} else {
				long err = bpf_map_update_elem(&blocked_by, &key,
							       &elapsed, BPF_NOEXIST);
				if (err == -EEXIST) {
					/* Another CPU filed the first interval
					 * for this thread and stack between the
					 * lookup and here. Add to what it
					 * created rather than calling a lost
					 * race a full map. */
					reason = bpf_map_lookup_elem(&blocked_by, &key);
					if (reason)
						*reason += elapsed;
				} else if (err) {
					bump(STAT_STACKS_LOST);
				}
			}
		}
		break;
	case STATE_EXITED:
		/* Frozen at the moment of exit. */
		break;
	default:
		t->unknown_ns += elapsed;
		break;
	}
}

/*
 * Move a thread into a new state, crediting the time it spent in the old one.
 *
 * This is the whole measurement. Everything else is plumbing that decides
 * which state a thread just moved into.
 *
 * arrival says what the event proves about the thread, and it is what keeps
 * dead threads dead.
 *
 * A switch-out proves the least: it may be a thread going to sleep, or it may
 * be the last thing a dying task ever does. Both look identical here, and the
 * second one leaves an entry that will never wake, never be scheduled again,
 * and accrue blocked time until the session ends -- an `echo` with zero
 * on-CPU and 12.02 seconds blocked, in a 12 second window.
 *
 * Refusing to create an entry from a switch-out was tried and is worse. A
 * thread whose first event is going to sleep is the ordinary case for
 * anything that waits, and not recording it loses precisely the blocked time
 * this tool exists to measure: the sleeper in the tests came back 94.6%
 * on-CPU, which is the same error as the phantom, pointing the other way.
 *
 * So a switch-out creates, and the exit program below plants a headstone
 * instead -- an entry already marked exited, which the dying task's last
 * switch then finds and leaves alone.
 *
 * Reviving an exited entry is stricter, and only sched_wakeup_new does it.
 * That event exists for exactly one thing, a task made runnable for the first
 * time, and it is the only unambiguous announcement that the tid now belongs
 * to somebody new.
 */
enum arrival {
	/* A switch-out. May update, may not create. */
	ARRIVAL_TAIL = 0,
	/* A switch-in or a wakeup: the thread is alive right now. */
	ARRIVAL_LIVE = 1,
	/* A wakeup_new: this task did not exist until now. */
	ARRIVAL_NEW = 2,
};
static __always_inline void transition(__u32 tid, __u32 tgid, const char *comm,
				       enum thread_state next_state, __u64 now,
				       enum arrival arrival, __u64 cgroup_id,
				       __s32 stack_id)
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
		struct thread fresh = {};
		start_fresh(&fresh, tid, tgid, comm, next_state, now);
		long err = bpf_map_update_elem(&threads, &tid, &fresh, BPF_NOEXIST);
		/* -EEXIST means another CPU saw this thread first, which is a
		 * race and not a full map. There is nothing to credit on a
		 * first sight anyway, so the entry it created is already
		 * correct. Counting it as capacity would have the report claim
		 * the map is at max_entries on a machine where it is nearly
		 * empty. */
		if (err && err != -EEXIST)
			bump(STAT_EVENTS_DROPPED);
		return;
	}

	if (t->state == STATE_EXITED) {
		if (arrival != ARRIVAL_NEW)
			return;
		start_fresh(t, tid, tgid, comm, next_state, now);
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

	/*
	 * Only some events can answer which cgroup a thread is in, so the
	 * answer is kept when it arrives. bpf_get_current_cgroup_id speaks for
	 * the running task, which at sched_switch is the one leaving -- so a
	 * thread learns its own cgroup when it is scheduled out and not when
	 * it is woken, where the running task is whoever did the waking. It is
	 * sticky in practice: threads change cgroup about as often as they
	 * change process.
	 */
	if (cgroup_id)
		t->cgroup_id = cgroup_id;

	/*
	 * The same rule, for the same reason: kept when an event that knows
	 * turns up, ignored when one that does not passes zero. Unlike the
	 * cgroup this cannot change over a thread's life at all -- a task's
	 * tgid is fixed unless it execs its way into being a group leader --
	 * so the only thing being waited for here is the first event able to
	 * say it.
	 */
	if (tgid)
		t->tgid = tgid;

	credit_elapsed(t, now);
	t->state = next_state;
	t->since_ns = now;
	/*
	 * Recorded after crediting, so the interval that just ended is filed
	 * against the stack it began on rather than the one starting now.
	 */
	t->block_stack_id = next_state == STATE_BLOCKED ? stack_id : -1;

	/*
	 * The clock on this wait starts here, and so does the reading it will
	 * be compared against.
	 */
	if (next_state == STATE_RUNQUEUE)
		t->throttle_snapshot_ns = throttled_upto_now(t->cgroup_id, now);
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
	__s32 stack_id = -1;

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

		/*
		 * The stack is taken here and nowhere else, because here is the
		 * only moment it says anything: the thread is still on the CPU
		 * it is leaving, and the frames underneath it are the call that
		 * decided to stop. One instant later they are gone.
		 *
		 * Only for a thread that is actually stopping. A preempted
		 * thread's stack describes whatever it was in the middle of,
		 * which is not a reason for anything.
		 */
		if (!runnable) {
			stack_id = bpf_get_stackid(ctx, &stacks, 0);
			if (stack_id < 0)
				bump(STAT_STACKS_LOST);
		}
		/* A switch-out: this is where a dying thread last switch
		 * arrives, moments after its exit event.
		 *
		 * The process comes from the same place the cgroup does, and is
		 * correct for the same reason: this tracepoint fires from
		 * inside __schedule before the registers are swapped, so
		 * current is still prev. It would be quietly wrong for next,
		 * which is why next is given nothing and waits for a switch of
		 * its own. */
		transition(prev, (__u32)(bpf_get_current_pid_tgid() >> 32), comm,
			   runnable ? STATE_RUNQUEUE : STATE_BLOCKED,
			   now, ARRIVAL_TAIL, bpf_get_current_cgroup_id(), stack_id);
	}

	if (tracked(next)) {
		__builtin_memcpy(comm, ctx->next_comm, sizeof(comm));
		transition(next, 0, comm, STATE_ON_CPU, now, ARRIVAL_LIVE, 0, -1);
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
	/* No process id: at a wakeup the running task is whoever did the
	 * waking, so bpf_get_current_pid_tgid would file this thread under
	 * someone else's process. */
	transition(tid, 0, comm, STATE_RUNQUEUE, bpf_ktime_get_ns(), ARRIVAL_LIVE, 0, -1);
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
	transition(tid, 0, comm, STATE_RUNQUEUE, bpf_ktime_get_ns(), ARRIVAL_NEW, 0, -1);
	return 0;
}

/*
 * Follow the threads a tracked process starts.
 *
 * Only its threads. sched_process_fork fires for fork() as well as for a
 * clone that shares the address space, so adding every child would follow the
 * target into whatever processes it spawns, and then into theirs -- a filter
 * that grows transitively through a build or a shell. A new thread shares its
 * parent's tgid; a forked process is its own group leader, and its tgid is its
 * own pid.
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

	if (BPF_CORE_READ(child_task, tgid) != BPF_CORE_READ(parent_task, tgid))
		return 0;

	__u32 child = (__u32)BPF_CORE_READ(child_task, pid);
	__u8 yes = 1;
	if (bpf_map_update_elem(&targets, &child, &yes, BPF_ANY) < 0)
		bump(STAT_TARGETS_FULL);
	return 0;
}

/*
 * The CFS bandwidth controller taking a cgroup off the CPU, and giving it
 * back.
 *
 * These are kernel functions rather than tracepoints, so they are hooked with
 * kprobes. fentry would be cheaper and is not worth it here: throttling fires
 * a handful of times per 100 ms period, where sched_switch fires tens of
 * thousands of times a second, so the saving is unmeasurable -- and both
 * functions are static in the kernel BTF, which makes fentry the more
 * fragile of the two for no return.
 *
 * A cgroup can be throttled on several CPUs, and this counts one episode per
 * cgroup rather than per runqueue: a thread waiting for a CPU cares that its
 * group was not allowed to run, not on how many cores that was true at once.
 * The first throttle opens the window and a later one while it is open leaves
 * it alone.
 */
SEC("kprobe/throttle_cfs_rq")
int BPF_KPROBE(on_throttle_cfs_rq, struct cfs_rq *cfs_rq)
{
	__u64 cgroup_id = BPF_CORE_READ(cfs_rq, tg, css.cgroup, kn, id);
	if (!cgroup_id)
		return 0;

	__u64 now = bpf_ktime_get_ns();
	struct throttle *th = bpf_map_lookup_elem(&throttled, &cgroup_id);
	if (!th) {
		struct throttle opening = { .since_ns = now };
		long err = bpf_map_update_elem(&throttled, &cgroup_id, &opening,
					       BPF_NOEXIST);
		if (err == -EEXIST) {
			/* A cgroup is throttled on several CPUs at once, so two
			 * of them racing to open the window is the normal case,
			 * not a capacity problem. */
			th = bpf_map_lookup_elem(&throttled, &cgroup_id);
			if (th && !th->since_ns)
				th->since_ns = now;
		} else if (err) {
			bump(STAT_CGROUPS_FULL);
		}
		return 0;
	}
	if (!th->since_ns)
		th->since_ns = now;
	return 0;
}

SEC("kprobe/unthrottle_cfs_rq")
int BPF_KPROBE(on_unthrottle_cfs_rq, struct cfs_rq *cfs_rq)
{
	__u64 cgroup_id = BPF_CORE_READ(cfs_rq, tg, css.cgroup, kn, id);
	if (!cgroup_id)
		return 0;

	struct throttle *th = bpf_map_lookup_elem(&throttled, &cgroup_id);
	if (!th || !th->since_ns)
		return 0;

	/*
	 * The window is closed before its length is banked, and the order is
	 * deliberate. A thread on another CPU reads total_ns and since_ns as
	 * though they were one value; between the two writes it sees either the
	 * episode counted twice, or not yet counted at all. The first inflates
	 * the baseline a thread records when it joins the runqueue and makes
	 * its throttled time come out too low -- silently, in the one number
	 * this phase exists to produce. The second costs at most one episode
	 * that the next read picks up.
	 */
	__u64 now = bpf_ktime_get_ns();
	__u64 opened = th->since_ns;
	th->since_ns = 0;
	if (now > opened)
		th->total_ns += now - opened;
	th->episodes += 1;
	return 0;
}

/*
 * Close a thread's books when it exits.
 *
 * This fires before the dying task's final context switch, which is what
 * makes the ordering work: the entry is marked exited here, and the switch
 * that follows finds it in that state and leaves it alone. Without this the
 * entry stays parked in whatever it was last doing and accrues blocked time
 * for the rest of the session -- so a process that lived a millisecond is
 * reported, entirely plausibly, as having been blocked for the whole window.
 */
SEC("raw_tp/sched_process_exit")
int BPF_PROG(on_sched_process_exit, struct task_struct *task)
{
	__u32 tid = (__u32)BPF_CORE_READ(task, pid);
	if (!tracked(tid))
		return 0;

	__u64 now = bpf_ktime_get_ns();

	/*
	 * The name comes from the task itself rather than from whatever the
	 * last scheduler event happened to carry. A process that forks and
	 * execs is created under its parent's name and only acquires its own
	 * at the exec; if it is short-lived enough that no transition arrives
	 * in between, every record of it would be filed under the name of the
	 * shell that started it. This is the last chance to get it right, and
	 * the task is right here.
	 */
	char comm[16];
	BPF_CORE_READ_INTO(&comm, task, comm);

	/*
	 * And the process, from the same task, for a related reason. Every
	 * other program has to infer this from whichever task happens to be
	 * running; here it is a field of the thread that is dying. A thread
	 * that lived entirely between two of its own switches -- so briefly
	 * that it was never prev at a sched_switch -- would otherwise be
	 * reported with no process at all, and short-lived threads are exactly
	 * what a report about process churn is made of.
	 */
	__u32 tgid = (__u32)BPF_CORE_READ(task, tgid);

	struct thread *t = bpf_map_lookup_elem(&threads, &tid);
	if (!t) {
		/*
		 * Never seen, and it is about to switch out for the last time.
		 * A headstone goes in so that the switch finds something
		 * already marked exited and leaves it there; without it, that
		 * switch creates a live-looking entry for a thread that is
		 * already gone, and nothing will ever close it. It carries no
		 * time -- there is none to report -- and userspace drains it
		 * on the next read.
		 */
		struct thread headstone = {};
		start_fresh(&headstone, tid, tgid, comm, STATE_EXITED, now);
		long err = bpf_map_update_elem(&threads, &tid, &headstone,
					       BPF_NOEXIST);
		if (err == -EEXIST) {
			/* It came into existence between the lookup and here.
			 * Mark that entry instead, or the switch that follows
			 * this exit finds a live-looking thread and keeps it
			 * alive forever. */
			t = bpf_map_lookup_elem(&threads, &tid);
			if (t) {
				t->tgid = tgid;
				credit_elapsed(t, now);
				t->state = STATE_EXITED;
				t->since_ns = now;
			}
		} else if (err) {
			bump(STAT_EVENTS_DROPPED);
		}
		return 0;
	}

	__builtin_memcpy(&t->comm, comm, sizeof(t->comm));
	t->tgid = tgid;
	credit_elapsed(t, now);
	t->state = STATE_EXITED;
	t->since_ns = now;

	/*
	 * Dropped from the target set as well, so that the next thread to be
	 * handed this tid is not followed on the strength of having inherited
	 * a number.
	 */
	if (filter_targets)
		bpf_map_delete_elem(&targets, &tid);
	return 0;
}

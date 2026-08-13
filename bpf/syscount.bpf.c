// SPDX-License-Identifier: GPL-2.0
/*
 * Syscall entries, counted in the kernel and streamed to userspace, from the
 * same tracepoint.
 *
 * Both models are here on purpose. Aggregating in a map costs one atomic add
 * per event and one read per reporting interval; streaming costs a ring
 * buffer reservation, a copy, a wakeup and userspace work per event, and
 * starts dropping when the consumer falls behind. Phase 2 attaches to
 * sched_switch, which fires tens of thousands of times a second on a busy
 * machine, so the difference stops being academic -- and the way to know
 * which one to reach for is to have measured both.
 */
#include <linux/bpf.h>
#include <linux/errno.h>
#include <bpf/bpf_helpers.h>

char LICENSE[] SEC("license") = "Dual BSD/GPL";

/*
 * The context passed to raw_syscalls:sys_enter.
 *
 * Declared here rather than pulled from vmlinux.h because a tracepoint's
 * layout is stable ABI: it is generated from TRACE_EVENT macros, userspace
 * reads it out of /sys/kernel/tracing/events/.../format, and the kernel does
 * not reorder it between versions. Ordinary kernel structs are the opposite
 * -- task_struct moves under every config change -- which is what CO-RE is
 * for, and phase 2 is where that starts to matter.
 *
 * The first member stands in for struct trace_entry, which is eight bytes of
 * type, flags, preempt count and pid that nothing here needs.
 */
struct sys_enter_ctx {
	__u64 __common;
	long syscall_nr;
	unsigned long args[6];
};

/*
 * Filter configuration, written by the loader before the program is loaded.
 *
 * const volatile is the contract: const so the verifier treats each one as a
 * known value and can fold the comparisons below into nothing when a filter
 * is off, volatile so clang does not decide the initialiser is the final
 * answer and optimise the read away entirely. The loader patches the .rodata
 * section before load, which is why a program filtered to one pid is not
 * merely quiet about the others -- it never executes their branch.
 *
 * Filtering here rather than in userspace is the whole point. A tracepoint on
 * sys_enter fires for every syscall on the machine; sending all of them up so
 * userspace can throw most away would cost more than the thing being
 * measured.
 */
const volatile __u32 target_tgid = 0;    /* 0: every process */
const volatile __u64 target_cgroup = 0;  /* 0: every cgroup */
const volatile long target_syscall = -1; /* -1: every syscall */

/*
 * Bounded on purpose, and the bound is visible. A hash map that fills up
 * fails silently: counts stop appearing for processes first seen after it
 * filled, and nothing in the output says so. The insert failure below is
 * counted and reported instead.
 */
#define MAX_TRACKED_PROCESSES 10240

/*
 * The name is carried here rather than looked up in /proc afterwards, and
 * that is not an optimisation. bpf_get_current_pid_tgid returns the pid in
 * the *initial* namespace; a tool running inside a container or a WSL distro
 * reads a /proc that numbers processes differently, so every lookup either
 * misses or -- worse -- lands on an unrelated process. The kernel knows the
 * name at the moment of the event, so the kernel is asked.
 *
 * It costs one 16-byte copy per process rather than per event, because it is
 * only written on the insert that first sees a process. The name is therefore
 * the one it had when first seen: a process that execs later keeps the old
 * name here, which is the correct trade for a counter and would not be for an
 * audit log.
 */
struct count {
	__u64 entries;
	char comm[16];
};

struct {
	__uint(type, BPF_MAP_TYPE_HASH);
	__uint(max_entries, MAX_TRACKED_PROCESSES);
	__type(key, __u32); /* thread group id, which is what ps calls a pid */
	__type(value, struct count);
} counts SEC(".maps");

struct event {
	__u64 timestamp_ns;
	__u32 tgid;
	__u32 pid; /* the kernel's pid: one thread */
	__s64 syscall_nr;
	char comm[16];
};

/*
 * Never read, and it has to be here. clang only emits BTF for types it
 * actually needs, and a struct that is only ever pointed at through the
 * return of bpf_ringbuf_reserve does not qualify -- so the layout never
 * reaches the object, and bpf2go has nothing to generate the Go mirror from.
 * A declared variable of the type is what forces it in.
 */
const struct event *unused_event __attribute__((unused));

/*
 * 256 KiB holds roughly 6500 events of this size. The ring buffer is shared
 * across CPUs rather than per-CPU like a perf buffer, so events stay ordered
 * relative to each other and the memory is not multiplied by the core count.
 */
struct {
	__uint(type, BPF_MAP_TYPE_RINGBUF);
	__uint(max_entries, 256 * 1024);
} events SEC(".maps");

/* Everything this program throws away, counted rather than shrugged off. */
enum stat_slot {
	STAT_RINGBUF_FULL = 0,
	STAT_COUNTS_FULL = 1,
	STAT__MAX = 2,
};

/* Forced into BTF for the same reason as unused_event above: the enum is only
 * ever used as an integer constant, so nothing would otherwise record that
 * these names exist, and userspace would have to hardcode 0 and 1. */
const enum stat_slot *unused_stat_slot __attribute__((unused));

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
	 * check: it knows the helper's return type is a pointer that may be
	 * null, and it will not take anyone's word that this call site is
	 * different.
	 */
	__u64 *counter = bpf_map_lookup_elem(&stats, &slot);
	if (counter)
		__sync_fetch_and_add(counter, 1);
}

static __always_inline int selected(__u32 tgid, long syscall_nr)
{
	if (target_tgid && tgid != target_tgid)
		return 0;
	if (target_syscall >= 0 && syscall_nr != target_syscall)
		return 0;
	/*
	 * Last because it is the only one that costs a helper call. The cheap
	 * comparisons above reject the common case first.
	 */
	if (target_cgroup && bpf_get_current_cgroup_id() != target_cgroup)
		return 0;
	return 1;
}

SEC("tracepoint/raw_syscalls/sys_enter")
int count_syscalls(struct sys_enter_ctx *ctx)
{
	__u64 id = bpf_get_current_pid_tgid();
	__u32 tgid = id >> 32;

	if (!selected(tgid, ctx->syscall_nr))
		return 0;

	struct count *count = bpf_map_lookup_elem(&counts, &tgid);
	if (count) {
		/*
		 * Atomic, not count->entries += 1. The same process runs on
		 * several CPUs at once and each has its own view; a plain
		 * read-modify-write loses increments under exactly the load this
		 * tool will be pointed at.
		 */
		__sync_fetch_and_add(&count->entries, 1);
		return 0;
	}

	struct count first = { .entries = 1 };
	bpf_get_current_comm(&first.comm, sizeof(first.comm));

	long err = bpf_map_update_elem(&counts, &tgid, &first, BPF_NOEXIST);
	if (err == -EEXIST) {
		/*
		 * Another CPU inserted this key between the lookup and here.
		 * Without this retry the first syscall a process makes on each
		 * racing CPU is silently dropped -- a small error, but one that
		 * grows with core count and would never show up in a test.
		 */
		count = bpf_map_lookup_elem(&counts, &tgid);
		if (count)
			__sync_fetch_and_add(&count->entries, 1);
	} else if (err) {
		bump(STAT_COUNTS_FULL);
	}
	return 0;
}

SEC("tracepoint/raw_syscalls/sys_enter")
int stream_syscalls(struct sys_enter_ctx *ctx)
{
	__u64 id = bpf_get_current_pid_tgid();
	__u32 tgid = id >> 32;

	if (!selected(tgid, ctx->syscall_nr))
		return 0;

	/*
	 * Reserve, fill, submit. The reservation is what makes the ring buffer
	 * better than the perf buffer here: the event is written straight into
	 * its final place, and a full buffer is discovered before anything is
	 * copied rather than after.
	 */
	struct event *e = bpf_ringbuf_reserve(&events, sizeof(*e), 0);
	if (!e) {
		bump(STAT_RINGBUF_FULL);
		return 0;
	}

	e->timestamp_ns = bpf_ktime_get_ns();
	e->tgid = tgid;
	e->pid = (__u32)id;
	e->syscall_nr = ctx->syscall_nr;
	bpf_get_current_comm(&e->comm, sizeof(e->comm));

	bpf_ringbuf_submit(e, 0);
	return 0;
}

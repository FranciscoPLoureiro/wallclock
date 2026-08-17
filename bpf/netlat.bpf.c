// SPDX-License-Identifier: GPL-2.0
/*
 * Who a thread is waiting for, and how long the answer took.
 *
 * "Blocked on network 22%" is half an answer. The other half is *whose*
 * network -- the database, the cache, the broker, or something nobody
 * remembered was in the path.
 *
 * The obvious way to get it is to note which destination each thread last
 * talked to and, when that thread blocks, charge the wait to that
 * destination. Phase 3 measured what that would produce on a modern stack and
 * the answer was: almost nothing. A Go client parks goroutines in a netpoller
 * and never stops a thread; PostgreSQL backends and Redis wait in epoll. Over
 * a whole machine under load, six threads recorded any network-blocked time
 * at all, 164 ms between them, every one a short-lived one-shot client.
 *
 * So this measures something a blocked thread is not required for: the time
 * between sending to a destination and receiving the first bytes back from
 * it. That interval exists whether the thread waited in the kernel, parked a
 * goroutine, or polled, because it is a property of the conversation rather
 * than of the scheduler.
 *
 * It is also keyed by the *connection* and not by the thread, which the
 * original plan called for and which measurement rejected: a goroutine sends
 * on one thread and is resumed on another, so a thread-keyed answer finds no
 * question. Of twenty exchanges against a known server, six paired that way
 * and twenty pair this way.
 *
 * What this is not: a general request-latency measure. It pairs a send with
 * the next receive on the same connection, which is what a request/response
 * protocol does. A connection streaming in one direction, or pipelining
 * several requests before reading any reply, is measured as something else,
 * and the report says so rather than pretending otherwise.
 */
#include <linux/bpf.h>
#include <linux/errno.h>
#include <linux/ptrace.h>
#include <bpf/bpf_helpers.h>
#include <bpf/bpf_core_read.h>
#include <bpf/bpf_tracing.h>

char LICENSE[] SEC("license") = "Dual BSD/GPL";

/*
 * Enough of struct sock to name the far end of a connection, and not one
 * field more.
 *
 * This is the declaration phase 4 exists to justify. In the real kernel
 * skc_daddr and skc_dport do not sit where this says they do: they are
 * members of anonymous unions inside struct sock_common, beside aliases that
 * let the kernel compare an address and a port in a single 64-bit load. Their
 * byte offsets move between kernel versions and between build configurations,
 * and nothing warns when they do -- a program compiled against the wrong
 * offset reads whatever is there and reports a plausible address that belongs
 * to nobody.
 *
 * preserve_access_index is what makes that safe. clang records "the field
 * named skc_daddr inside struct sock_common" instead of "twelve bytes in",
 * and libbpf rewrites the access against the running kernel's own BTF before
 * the verifier sees it. Matching is by name, so the fields left out here
 * cannot be got wrong, and the anonymous unions do not need declaring at all
 * because BTF flattens anonymous members into their parent.
 *
 * That is the whole of CO-RE in one struct: the binary works on a kernel it
 * was never compiled against, and it fails loudly rather than quietly if the
 * field it needs has actually gone.
 */
struct sock_common {
	__be32 skc_daddr;
	__be16 skc_dport;
} __attribute__((preserve_access_index));

struct sock {
	struct sock_common __sk_common;
} __attribute__((preserve_access_index));

/* A destination: the far end of a TCP connection, in network byte order, as
 * the kernel holds it. Userspace does the swapping, because the kernel side
 * should do as little as possible per event. */
struct destination {
	__be32 daddr;
	__be16 dport;
	__u16 pad;
};

/* What a connection is in the middle of: a request sent to somebody, with no
 * answer back yet. */
struct in_flight {
	struct destination to;
	__u64 sent_ns;
};

/*
 * Latency to one destination, as a log2 histogram of microseconds plus the
 * totals a mean needs.
 *
 * A histogram rather than a mean, because the mean of a latency distribution
 * is the one summary that hides the thing anybody is looking for. The totals
 * are kept beside it so the report can print a mean without walking the
 * buckets, and the maximum because a single worst case is often the reason
 * somebody opened the tool.
 */
#define MAX_SLOTS 32

struct dest_stats {
	__u64 count;
	__u64 total_ns;
	__u64 max_ns;
	__u64 slots[MAX_SLOTS];
};

#define MAX_THREADS 16384
#define MAX_SOCKETS 16384
#define MAX_DESTINATIONS 4096

/*
 * Outstanding requests, keyed by the socket rather than by the thread.
 *
 * The phase was specified around a map from TID to the conversation in
 * progress, and that key is wrong -- measurably. A Go client sends on
 * whichever thread its goroutine is running on, parks in the netpoller, and
 * is resumed on whichever thread is free, which is usually a different one.
 * Keyed by thread, the answer then finds no question: of twenty exchanges
 * against a known server, six paired. Keyed by the socket, all twenty do,
 * because a connection does not move between threads even when the goroutine
 * using it does.
 *
 * A socket address can be reused after the socket is freed. An entry is
 * removed as soon as its answer arrives, so what can go stale is a request
 * that never got one -- and the destination is checked against the reply's
 * before anything is recorded.
 */
struct {
	__uint(type, BPF_MAP_TYPE_HASH);
	__uint(max_entries, MAX_SOCKETS);
	__type(key, __u64); /* struct sock *, as an integer */
	__type(value, struct in_flight);
} in_flight SEC(".maps");

/* Which socket a thread is reading from right now, recorded on the way into
 * tcp_recvmsg so that the return probe -- which has no arguments left to read
 * a socket from -- knows which conversation just answered. This one really is
 * per thread: a thread is inside exactly one read at a time. */
struct {
	__uint(type, BPF_MAP_TYPE_HASH);
	__uint(max_entries, MAX_THREADS);
	__type(key, __u32);
	__type(value, __u64);
} recv_sock SEC(".maps");

struct {
	__uint(type, BPF_MAP_TYPE_HASH);
	__uint(max_entries, MAX_DESTINATIONS);
	__type(key, struct destination);
	__type(value, struct dest_stats);
} destinations SEC(".maps");

/* Counters, so that what could not be recorded is reported rather than
 * absorbed. Same reasoning as the offcpu program: a tool that silently drops
 * what it cannot fit lies with confidence. */
enum stat_slot {
	STAT_DESTINATIONS_FULL = 0,
	STAT_THREADS_FULL = 1,
	STAT_UNPAIRED = 2,
	STAT_SOCKETS_FULL = 3,
	STAT_SLOT_MAX = 4,
};

struct {
	__uint(type, BPF_MAP_TYPE_ARRAY);
	__uint(max_entries, STAT_SLOT_MAX);
	__type(key, __u32);
	__type(value, __u64);
} stats SEC(".maps");

/* Types bpf2go is asked to generate Go declarations for. Without a reference
 * they are dropped from the BTF as unused and the generator cannot find them. */
const struct destination *unused_destination __attribute__((unused));
const struct dest_stats *unused_dest_stats __attribute__((unused));
const enum stat_slot *unused_stat_slot __attribute__((unused));

/* Zero watches everything; anything else watches one cgroup. Set from
 * userspace before load, so the filter costs a comparison rather than a map
 * lookup, and the cgroup id is the one filter that means the same thing
 * inside a container and out. */
const volatile __u64 target_cgroup = 0;

static __always_inline void bump(__u32 slot)
{
	__u64 *counter = bpf_map_lookup_elem(&stats, &slot);
	if (counter)
		__sync_fetch_and_add(counter, 1);
}

static __always_inline int watched(void)
{
	if (!target_cgroup)
		return 1;
	return bpf_get_current_cgroup_id() == target_cgroup;
}

/* log2 of a 32-bit value, branchless, so the verifier does not have to reason
 * about a loop. */
static __always_inline __u32 log2_32(__u32 v)
{
	__u32 r, shift;

	r = (v > 0xFFFF) << 4;
	v >>= r;
	shift = (v > 0xFF) << 3;
	v >>= shift;
	r |= shift;
	shift = (v > 0xF) << 2;
	v >>= shift;
	r |= shift;
	shift = (v > 0x3) << 1;
	v >>= shift;
	r |= shift;
	r |= (v >> 1);
	return r;
}

static __always_inline __u32 log2_64(__u64 v)
{
	__u32 hi = v >> 32;

	if (hi)
		return 32 + log2_32(hi);
	return log2_32((__u32)v);
}

static __always_inline void read_destination(struct sock *sk, struct destination *to)
{
	to->daddr = BPF_CORE_READ(sk, __sk_common.skc_daddr);
	to->dport = BPF_CORE_READ(sk, __sk_common.skc_dport);
	to->pad = 0;
}

/*
 * A request going out. The clock on this conversation starts here.
 *
 * Overwritten rather than accumulated when a thread sends twice without
 * reading: the interval that means something is from the *last* thing said to
 * the first thing heard back. A thread that pipelines is measured as the
 * latency of its last request, which is wrong for pipelining and is the
 * documented limit of this approach rather than a bug hiding in it.
 */
SEC("kprobe/tcp_sendmsg")
int BPF_KPROBE(on_tcp_sendmsg, struct sock *sk)
{
	if (!watched())
		return 0;

	struct in_flight flight = {};

	read_destination(sk, &flight.to);
	/* An unconnected socket has no far end to attribute anything to. */
	if (!flight.to.daddr)
		return 0;
	flight.sent_ns = bpf_ktime_get_ns();

	__u64 key = (__u64)(unsigned long)sk;

	if (bpf_map_update_elem(&in_flight, &key, &flight, BPF_ANY) < 0)
		bump(STAT_SOCKETS_FULL);
	return 0;
}

/*
 * Entering a read. Only the destination is recorded here, because whether
 * anything arrives is not known until the call returns.
 */
SEC("kprobe/tcp_recvmsg")
int BPF_KPROBE(on_tcp_recvmsg, struct sock *sk)
{
	if (!watched())
		return 0;

	__u32 tid = (__u32)bpf_get_current_pid_tgid();
	__u64 sock = (__u64)(unsigned long)sk;

	if (bpf_map_update_elem(&recv_sock, &tid, &sock, BPF_ANY) < 0)
		bump(STAT_THREADS_FULL);
	return 0;
}

/*
 * The answer arriving, which is the only moment that can close the interval.
 *
 * A return probe rather than a second entry probe, because entering
 * tcp_recvmsg says a thread would like some data and returning from it says
 * some data exists. For a Go client those are very different instants: the
 * runtime calls in, is told there is nothing, parks the goroutine, and comes
 * back later -- and it is the later call, the one that returns bytes, that
 * ends the wait.
 *
 * A non-positive return is not an answer. Zero is end of stream and negative
 * is an error, most often EAGAIN on a non-blocking socket with nothing ready,
 * which is precisely the call a netpolling runtime makes constantly and must
 * not be counted as a reply.
 */
SEC("kretprobe/tcp_recvmsg")
int BPF_KRETPROBE(on_tcp_recvmsg_ret, int ret)
{
	if (!watched())
		return 0;

	__u32 tid = (__u32)bpf_get_current_pid_tgid();
	__u64 *sock = bpf_map_lookup_elem(&recv_sock, &tid);

	if (!sock)
		return 0;
	if (ret <= 0)
		return 0;

	__u64 key = *sock;
	struct in_flight *flight = bpf_map_lookup_elem(&in_flight, &key);

	if (!flight) {
		/* Data arrived on a connection nobody sent a request on -- a
		 * server reading a request, or the second half of a reply
		 * whose first half already closed the interval. Neither is an
		 * error and neither is a measurement. */
		bump(STAT_UNPAIRED);
		return 0;
	}

	__u64 now = bpf_ktime_get_ns();

	if (now > flight->sent_ns) {
		__u64 elapsed = now - flight->sent_ns;
		struct destination key = flight->to;
		struct dest_stats *stats_for = bpf_map_lookup_elem(&destinations, &key);

		if (!stats_for) {
			struct dest_stats fresh = {};
			long err = bpf_map_update_elem(&destinations, &key, &fresh, BPF_NOEXIST);

			/* -EEXIST is another CPU having created it a moment
			 * ago, which is a race and not a full map. Counting it
			 * as capacity would claim max_entries on a machine
			 * talking to three services. */
			if (err && err != -EEXIST) {
				bump(STAT_DESTINATIONS_FULL);
				goto done;
			}
			stats_for = bpf_map_lookup_elem(&destinations, &key);
			if (!stats_for)
				goto done;
		}

		__sync_fetch_and_add(&stats_for->count, 1);
		__sync_fetch_and_add(&stats_for->total_ns, elapsed);
		if (elapsed > stats_for->max_ns)
			stats_for->max_ns = elapsed;

		/* Microseconds, because a log2 histogram of nanoseconds spends
		 * ten buckets on intervals no network can produce. */
		__u32 slot = log2_64(elapsed / 1000);

		if (slot >= MAX_SLOTS)
			slot = MAX_SLOTS - 1;
		__sync_fetch_and_add(&stats_for->slots[slot], 1);
	}

done:
	/* The conversation is closed either way: leaving it open would pair
	 * this thread's next reply with a request two exchanges old. */
	bpf_map_delete_elem(&in_flight, &key);
	return 0;
}

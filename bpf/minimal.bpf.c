// SPDX-License-Identifier: GPL-2.0
/*
 * The smallest program that still proves something.
 *
 * Phase 0 needs one question answered before any of the real work is worth
 * planning: does the toolchain produce an object that a real kernel accepts,
 * here and on a GitHub Actions runner. Two instructions are enough to answer
 * it, and small enough that a rejection can only mean the environment is
 * wrong -- never the program.
 *
 * It is a tracepoint program rather than a socket filter on purpose. Every
 * program this project will grow attaches to the scheduler tracepoints, and
 * loading that class requires CAP_BPF *and* CAP_PERFMON, where a socket
 * filter needs only CAP_BPF. Proving the weaker requirement would prove the
 * wrong thing.
 */
#include <linux/bpf.h>
#include <bpf/bpf_helpers.h>

/*
 * The license declared here is the program's, not the repository's, and the
 * kernel enforces it: helpers marked GPL-only -- bpf_probe_read_kernel and
 * bpf_get_stackid among them, both of which phase 2 depends on -- refuse to
 * link into a program that does not declare a GPL-compatible license. This
 * program calls no helpers at all, but declaring it now keeps the rule
 * visible at the point where forgetting it would cost an afternoon.
 */
char LICENSE[] SEC("license") = "Dual BSD/GPL";

SEC("tracepoint/syscalls/sys_enter_getpid")
int wallclock_minimal(void *ctx)
{
	return 0;
}

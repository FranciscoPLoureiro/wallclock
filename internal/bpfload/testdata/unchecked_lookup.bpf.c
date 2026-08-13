// SPDX-License-Identifier: GPL-2.0
/*
 * A program the verifier must refuse, kept as a fixture.
 *
 * This is the mistake, not an invented one: the first version of the counting
 * program in bpf/syscount.bpf.c dereferenced the result of
 * bpf_map_lookup_elem without checking it, because in ordinary C a lookup
 * into a map you have just guaranteed is populated obviously returns
 * something. The verifier does not accept "obviously".
 *
 * It is a fixture rather than a note in a document because a rejection
 * recorded in prose stops being true the moment a kernel changes its mind,
 * and nobody would notice. The test that loads this asserts the rejection,
 * so the claim in the README is checked on every run.
 *
 * What the verifier says, verbatim, on 6.6.87.2-microsoft-standard-WSL2:
 *
 *   load program: permission denied:
 *       reg type unsupported for arg#0 function unchecked_lookup#1
 *       R0 invalid mem access 'map_value_or_null'
 *       verification time 26 usec
 *       stack depth 8
 *       processed 8 insns (limit 1000000) ...
 *
 * The operative line is the second. map_value_or_null is the verifier's name
 * for what bpf_map_lookup_elem returns: either a pointer into the map or
 * NULL, and it will not let the program touch it until the branch that tells
 * the two apart has been taken. "permission denied" as the errno is a red
 * herring -- EACCES is what a rejected load returns regardless of the reason,
 * which is why the log matters and the error code does not.
 *
 * The fix is the null check in count_syscalls: two lines, and the reason the
 * verifier exists. The helper genuinely can return NULL -- the key may not be
 * in the map yet -- and a kernel that took the program's word for it would
 * dereference address zero in interrupt context.
 */
#include <linux/bpf.h>
#include <bpf/bpf_helpers.h>

char LICENSE[] SEC("license") = "Dual BSD/GPL";

struct {
	__uint(type, BPF_MAP_TYPE_HASH);
	__uint(max_entries, 1024);
	__type(key, __u32);
	__type(value, __u64);
} counts SEC(".maps");

SEC("tracepoint/raw_syscalls/sys_enter")
int unchecked_lookup(void *ctx)
{
	__u32 tgid = bpf_get_current_pid_tgid() >> 32;
	__u64 *count = bpf_map_lookup_elem(&counts, &tgid);

	/* The missing line is: if (!count) return 0; */
	*count += 1;
	return 0;
}

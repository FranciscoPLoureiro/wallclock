# Shared by scripts/kernel-matrix.sh and scripts/distro-matrix.sh: the rule
# that turns one guest's log into one row.
#
# One copy, because the two drivers boot very different guests -- a bare
# ci-kernels image under vimto, and a distribution's cloud image over ssh --
# and the thing that must not differ between them is what counts as a pass.
# Two copies of this would eventually disagree about that, and the table would
# be comparing two standards while looking like it compared kernels.

FLOOR_MAJOR=${FLOOR_MAJOR:-5}
FLOOR_MINOR=${FLOOR_MINOR:-8}
MATRIX_PACKAGES=${MATRIX_PACKAGES:-"bpfload syscount offcpu netlat"}

# matrix_evaluate <log> <driver-exit-status> <deadline-seconds>
#
# Echoes one "release|task_struct|result|notes" row. Returns 0 if the row held
# and 1 if it did not.
matrix_evaluate() {
	local log=$1 status=$2 deadline=$3

	# Nothing below is read unless the guest said it reached the end. A row
	# whose evidence is a truncated log is not a result, whatever exit status
	# it happens to carry -- the previous attempt at this matrix recorded a
	# green refusal for a kernel the tool had never run on, because the guest
	# hung, the deadline killed it, and the word the assertion matched
	# appeared in the timeout message.
	if ! grep -q '^MATRIX_DONE$' "$log"; then
		if [ "$status" -eq 124 ]; then
			echo "-|-|no result|guest did not finish within ${deadline}s"
		else
			echo "-|-|no result|guest exited $status before finishing"
		fi
		return 1
	fi

	local release members preflight
	release=$(sed -n 's/^MATRIX_UNAME=//p' "$log" | head -1)
	members=$(sed -n 's/.*task_struct has \([0-9]*\) members.*/\1/p' "$log" | head -1)
	preflight=$(sed -n 's/^MATRIX_PREFLIGHT_EXIT=//p' "$log" | head -1)
	[ -n "$members" ] || members="-"

	# The floor is decided from the kernel that booted, not from the image
	# that was asked for: floating tags move, and a row that reasoned from the
	# tag would eventually describe a kernel it did not run on.
	local major rest minor below=0
	major=${release%%.*}
	rest=${release#*.}
	minor=${rest%%.*}
	if [ "$major" -lt "$FLOOR_MAJOR" ] ||
		{ [ "$major" -eq "$FLOOR_MAJOR" ] && [ "$minor" -lt "$FLOOR_MINOR" ]; }; then
		below=1
	fi

	if [ "$below" -eq 1 ]; then
		# Not "it failed" -- nearly everything that goes wrong also fails.
		# That the kernel requirement is the one refused, on a run that
		# reached the end.
		if [ "$preflight" != "0" ] && grep -qE '^kernel >= [0-9.]+ +FAIL' "$log"; then
			echo "$release|$members|refused|correctly: below the ${FLOOR_MAJOR}.${FLOOR_MINOR} floor, refused on the kernel requirement"
			return 0
		fi
		echo "$release|$members|WRONG|below the floor and did not refuse on the kernel requirement"
		return 1
	fi

	if [ "$preflight" != "0" ]; then
		local missing
		missing=$(grep -E ' +FAIL ' "$log" | sed 's/ \{2,\}.*//' | paste -sd, -)
		echo "$release|$members|FAIL|preflight refused a supported kernel: $missing"
		return 1
	fi

	# Every package must have both begun and reported a status. One that began
	# and never reported died mid-run, and the absence of a failing status is
	# not a pass.
	local bad="" pkg pkgstatus
	for pkg in $MATRIX_PACKAGES; do
		pkgstatus=$(sed -n "s/^MATRIX_TEST_EXIT=$pkg://p" "$log" | head -1)
		if [ -z "$pkgstatus" ]; then
			bad="$bad $pkg:noresult"
		elif [ "$pkgstatus" != "0" ]; then
			bad="$bad $pkg:$pkgstatus"
		fi
	done
	if [ -n "$bad" ]; then
		echo "$release|$members|FAIL|tests failed:$bad"
		return 1
	fi

	# Skips are named, not counted. A row that passed with the throttling test
	# skipped and one where it ran are different results, and a count does not
	# say which this is -- the same mistake as a check that cannot fail, one
	# level up.
	local skipped
	skipped=$(grep -oE '^--- SKIP: [A-Za-z0-9_]+' "$log" | sed 's/^--- SKIP: //' | sort -u | paste -sd, -)
	local throttling
	throttling=$(grep -qE '^cgroup cpu bandwidth +ok' "$log" && echo yes || echo no)
	if [ -n "$skipped" ]; then
		echo "$release|$members|partial|throttling observable: $throttling; skipped: $skipped"
	else
		echo "$release|$members|pass|throttling observable: $throttling; nothing skipped"
	fi
	return 0
}

# matrix_print_table <row>...
matrix_print_table() {
	printf '%-22s %-12s %-9s %s\n' "kernel" "task_struct" "result" "notes"
	printf '%-22s %-12s %-9s %s\n' "----------------------" "------------" "---------" "-----"
	local row a b c d
	for row in "$@"; do
		IFS='|' read -r a b c d <<<"$row"
		printf '%-22s %-12s %-9s %s\n' "$a" "$b" "$c" "$d"
	done
}

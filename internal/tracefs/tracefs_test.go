package tracefs

import (
	"os"
	"testing"
)

// The fixtures are the real thing, copied verbatim out of two running kernels
// rather than written by hand. A hand-written example of the layout that broke
// this would be an example of what somebody believed the layout was, which is
// the mistake being tested for.
func layoutFrom(t *testing.T, name string) Layout {
	t.Helper()
	file, err := os.Open("testdata/" + name)
	if err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	defer file.Close()
	layout, err := parse(file, name)
	if err != nil {
		t.Fatalf("parse %s: %v", name, err)
	}
	return layout
}

// The offsets this project was compiled against.
func TestAMainlineLayoutParses(t *testing.T) {
	layout := layoutFrom(t, "sched_switch-arch-7.1.format")
	for _, want := range []struct {
		field  string
		offset uint32
		size   uint32
	}{
		{"prev_comm", 8, 16},
		{"prev_pid", 24, 4},
		{"prev_state", 32, 8},
		{"next_comm", 40, 16},
		{"next_pid", 56, 4},
	} {
		got, err := layout.Offset(want.field, want.size)
		if err != nil {
			t.Errorf("%s: %v", want.field, err)
			continue
		}
		if got != want.offset {
			t.Errorf("%s is at %d, want %d", want.field, got, want.offset)
		}
	}
}

// The layout that made this package necessary.
//
// Red Hat's 9.x kernel calls itself 5.14 and carries the PREEMPT_RT
// patchset's lazy preemption, which adds preempt_lazy_count to the common
// header. Everything after it moves -- by four bytes for the four-byte fields
// and by eight for prev_state, which has to be aligned. That difference is the
// reason this reads per-field offsets rather than one shift.
func TestTheRedHatLayoutMovesFieldsByDifferentAmounts(t *testing.T) {
	mainline := layoutFrom(t, "sched_switch-arch-7.1.format")
	rhel := layoutFrom(t, "sched_switch-rhel-9.format")

	if _, ok := rhel["common_preempt_lazy_count"]; !ok {
		t.Fatal("the fixture is supposed to be a kernel that has the lazy preempt count")
	}

	for _, want := range []struct {
		field  string
		offset uint32
		size   uint32
		moved  uint32
	}{
		{"prev_pid", 28, 4, 4},
		{"prev_state", 40, 8, 8},
		{"next_pid", 64, 4, 8},
	} {
		got, err := rhel.Offset(want.field, want.size)
		if err != nil {
			t.Errorf("%s: %v", want.field, err)
			continue
		}
		if got != want.offset {
			t.Errorf("%s is at %d, want %d", want.field, got, want.offset)
		}
		if moved := got - mainline[want.field].Offset; moved != want.moved {
			t.Errorf("%s moved by %d bytes, want %d -- if these were all equal a "+
				"single shift would do and this package would not need to exist",
				want.field, moved, want.moved)
		}
	}
}

// The one the syscall filter was reading from the wrong place.
func TestTheRedHatSysEnterPutsIdAtSixteen(t *testing.T) {
	layout := layoutFrom(t, "sys_enter-rhel-9.format")
	got, err := layout.Offset("id", 8)
	if err != nil {
		t.Fatalf("id: %v", err)
	}
	if got != 16 {
		t.Fatalf("id is at %d, want 16", got)
	}
}

// A field read at the right offset with the wrong width is the same silent
// wrongness in a smaller package, so the size is part of the lookup.
func TestAFieldOfTheWrongSizeIsRefused(t *testing.T) {
	layout := layoutFrom(t, "sched_switch-arch-7.1.format")
	if _, err := layout.Offset("prev_state", 4); err == nil {
		t.Fatal("prev_state is eight bytes and a four byte read was accepted")
	}
}

// The error a missing field produces has to name what the kernel does have,
// because "no field prev_pid" sends the reader looking for a typo and the
// answer is almost always that the tracepoint changed.
func TestAMissingFieldSaysWhatTheKernelHasInstead(t *testing.T) {
	layout := layoutFrom(t, "sched_switch-arch-7.1.format")
	_, err := layout.Offset("prev_pid_renamed", 4)
	if err == nil {
		t.Fatal("a field that does not exist was found")
	}
	if got := err.Error(); !contains(got, "prev_pid") {
		t.Errorf("the error does not list the fields that do exist: %s", got)
	}
}

func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}

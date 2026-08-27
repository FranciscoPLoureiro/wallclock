// Package tracefs reads a tracepoint's field layout from the kernel that is
// about to run the program.
//
// A BPF program attached to a formatted tracepoint reads its arguments at
// offsets fixed when it was compiled. Those offsets are not stable. Field
// *names* are: the kernel does not rename or reorder them. Where they sit
// does move, for two reasons that have both been seen here.
//
// The first is a field appearing in the middle. Red Hat's 9.x kernel carries
// PREEMPT_LAZY -- upstream in 6.13 -- backported into a kernel that still
// calls itself 5.14, and it adds common_preempt_lazy_count to the common
// header every tracepoint begins with. Everything after it moves, and not by
// a constant: on sched_switch prev_pid moves 24 -> 28 while prev_state, being
// eight bytes and needing alignment, moves 32 -> 40.
//
// The second is a field changing representation. On 6.17 sched_process_fork's
// parent_comm became a __data_loc dynamic string, four bytes instead of
// sixteen, and everything after it moved the other way.
//
// The second kind announces itself: the read runs past the end of the record
// and the kernel refuses to attach the program, with EACCES. The first does
// not. The record grew, every read stays inside it, and the program attaches,
// runs, and reports whatever happens to be at the old offset. On Rocky 9 that
// made wallclock report thread ids of 6911073 and 7102830 -- bytes of a comm
// read as an integer -- with a decomposition that closed to 100% and a report
// that said no threads were lost. Reading the offsets from the kernel that is
// about to load the program is what removes the possibility.
package tracefs

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/cilium/ebpf"
)

// Roots are tried in order. tracefs is normally mounted at the first; the
// second is where it lands when only debugfs was mounted, which is still the
// common arrangement on Red Hat derivatives.
var roots = []string{
	"/sys/kernel/tracing/events",
	"/sys/kernel/debug/tracing/events",
}

// Field is where one of a tracepoint's arguments sits, in the kernel doing
// the reporting.
type Field struct {
	Offset uint32
	Size   uint32
}

// Layout maps a tracepoint's field names to their positions.
type Layout map[string]Field

// Read parses the format file of one tracepoint, e.g. Read("sched",
// "sched_switch").
func Read(system, event string) (Layout, error) {
	var lastErr error
	for _, root := range roots {
		path := filepath.Join(root, system, event, "format")
		file, err := os.Open(path) //nolint:gosec // a fixed path under a pseudo-filesystem
		if err != nil {
			lastErr = err
			continue
		}
		defer file.Close()
		return parse(file, path)
	}
	return nil, fmt.Errorf("no format file for %s:%s: %w", system, event, lastErr)
}

// Offset returns the offset of one field, and says which tracepoint it was
// looking in when it cannot find it -- because the answer to "field not
// found" is almost always that the kernel renamed or dropped it, and the
// caller's error message is the only place that will ever be read.
func (l Layout) Offset(field string, size uint32) (uint32, error) {
	f, ok := l[field]
	if !ok {
		names := make([]string, 0, len(l))
		for name := range l {
			names = append(names, name)
		}
		return 0, fmt.Errorf("no field %q in this kernel's layout (it has: %s)",
			field, strings.Join(names, ", "))
	}
	// Size is checked as well as offset. A field that kept its name and
	// changed width -- which is what __data_loc did to parent_comm -- would
	// otherwise be read at the right place with the wrong length, which is
	// the same silent wrongness in a smaller package.
	if f.Size != size {
		return 0, fmt.Errorf("field %q is %d bytes in this kernel, and the program reads %d",
			field, f.Size, size)
	}
	return f.Offset, nil
}

func parse(r io.Reader, path string) (Layout, error) {
	layout := Layout{}
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "field:") {
			continue
		}
		// "field:char prev_comm[TASK_COMM_LEN];\toffset:12;\tsize:16;\tsigned:1;"
		parts := strings.Split(line, ";")
		if len(parts) < 3 {
			continue
		}
		decl := strings.TrimPrefix(parts[0], "field:")
		name := decl
		if i := strings.LastIndexAny(decl, " \t*"); i >= 0 {
			name = decl[i+1:]
		}
		// An array declares its length in the name -- prev_comm[TASK_COMM_LEN],
		// args[6] -- and the name the kernel answers to is what precedes it.
		if i := strings.IndexByte(name, '['); i >= 0 {
			name = name[:i]
		}
		offset, err1 := numberAfter(parts[1], "offset:")
		size, err2 := numberAfter(parts[2], "size:")
		if err1 != nil || err2 != nil {
			return nil, fmt.Errorf("parsing %s: unreadable field line %q", path, line)
		}
		layout[name] = Field{Offset: offset, Size: size}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	if len(layout) == 0 {
		return nil, fmt.Errorf("%s declared no fields", path)
	}
	return layout, nil
}

func numberAfter(s, prefix string) (uint32, error) {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, prefix) {
		return 0, fmt.Errorf("expected %q in %q", prefix, s)
	}
	n, err := strconv.ParseUint(strings.TrimPrefix(s, prefix), 10, 32)
	return uint32(n), err
}

// Binding ties a const volatile in a BPF program to the tracepoint field it
// is meant to locate.
type Binding struct {
	// Variable is the name of the const volatile in the program.
	Variable string
	// Field is the tracepoint field it holds the offset of.
	Field string
	// Size is how many bytes the program reads there. Checked against the
	// kernel, because a field that kept its name and changed width is read
	// at the right offset and still wrong.
	Size uint32
}

// Bind reads one tracepoint's layout from the running kernel and writes each
// field's offset into the program variable named for it.
//
// Every failure here is returned rather than defaulted around. A program that
// silently kept its compiled-in offsets would attach, run, and report the
// wrong numbers -- which is the failure this package exists to remove, and
// falling back to it on error would reintroduce it at exactly the moment
// something unexpected is true of the kernel.
func Bind(vars map[string]*ebpf.VariableSpec, system, event string, bindings []Binding) error {
	layout, err := Read(system, event)
	if err != nil {
		return err
	}
	for _, b := range bindings {
		offset, err := layout.Offset(b.Field, b.Size)
		if err != nil {
			return fmt.Errorf("%s:%s: %w", system, event, err)
		}
		v, ok := vars[b.Variable]
		if !ok {
			return fmt.Errorf("the object declares no variable %q", b.Variable)
		}
		if err := v.Set(offset); err != nil {
			return fmt.Errorf("setting %s to the offset of %s:%s %s: %w",
				b.Variable, system, event, b.Field, err)
		}
	}
	return nil
}

// SameLayout reports an error unless two tracepoints agree about the offset
// and size of every named field.
//
// One variable serves both sched_wakeup and sched_wakeup_new, which have
// always been declared from the same macro. "Always" is the kind of thing
// this package was written because of, so it is checked rather than assumed.
func SameLayout(system, a, b string, fields []string) error {
	la, err := Read(system, a)
	if err != nil {
		return err
	}
	lb, err := Read(system, b)
	if err != nil {
		return err
	}
	for _, f := range fields {
		fa, oka := la[f]
		fb, okb := lb[f]
		if !oka || !okb {
			return fmt.Errorf("%s:%s and %s:%s do not both have a field %q", system, a, system, b, f)
		}
		if fa != fb {
			return fmt.Errorf("%s:%s puts %s at offset %d size %d and %s:%s at offset %d size %d, "+
				"so one program variable cannot serve both",
				system, a, f, fa.Offset, fa.Size, system, b, fb.Offset, fb.Size)
		}
	}
	return nil
}

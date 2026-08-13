// Package bpfload puts a compiled BPF object into a running kernel and
// reports what the verifier said about it.
//
// It exists in phase 0 because compiling is not the interesting half. clang
// will happily produce an object the kernel refuses: the verifier enforces
// bounded loops, a 512-byte stack, an instruction ceiling and a proof that
// every pointer was checked before it was read, and none of that is visible
// until the load. A build pipeline that only compiles proves nothing, so this
// package is what CI runs to make the proof real.
package bpfload

import (
	"errors"
	"fmt"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/rlimit"
)

// Program describes one program as it reached the kernel.
type Program struct {
	// Name is the C function name, which is also the key in the collection.
	Name string
	// Type is the program type the section header asked for.
	Type ebpf.ProgramType
	// Instructions is the count after clang optimised and the loader
	// rewrote relocations -- not the number of lines of C.
	Instructions int
	// VerifierLog is the kernel's own account of the load. It holds the
	// "processed N insns" line, which is the only first-hand evidence that a
	// program reached the verifier rather than being skipped.
	VerifierLog string
}

// Loaded owns the kernel objects created from one ELF. Close releases them;
// until then the programs stay resident even if nothing is attached.
type Loaded struct {
	Programs []Program

	coll *ebpf.Collection
}

// Object loads every program in the ELF at path into the kernel.
func Object(path string) (*Loaded, error) {
	// Before 5.11 BPF memory is charged against RLIMIT_MEMLOCK, and the
	// default limit rejects anything past a toy map. From 5.11 it is charged
	// to the memory cgroup and this does nothing.
	//
	// Its failure is not fatal here. Raising the limit needs
	// CAP_SYS_RESOURCE, so an unprivileged process fails at this line before
	// reaching anything that would tell it something useful -- and then every
	// error out of this package, including "that file is not an ELF", arrives
	// as a complaint about a memory limit. It is kept and reported only if
	// the load itself then fails, where it might actually be the cause.
	memlockErr := rlimit.RemoveMemlock()

	spec, err := ebpf.LoadCollectionSpec(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}

	// LogLevelStats asks the verifier for its summary even when the load
	// succeeds. It costs a buffer and gives up the library's retry-on-failure
	// path, and it buys the one line that turns "the test passed" into "the
	// kernel processed 2 instructions" -- which is the difference between a
	// green tick and evidence.
	coll, err := ebpf.NewCollectionWithOptions(spec, ebpf.CollectionOptions{
		Programs: ebpf.ProgramOptions{LogLevel: ebpf.LogLevelStats},
	})
	if err != nil {
		hint := ""
		if memlockErr != nil {
			hint = " (RLIMIT_MEMLOCK could not be raised either: " + memlockErr.Error() + ")"
		}
		// Wrapped, not rendered. A rejection carries the verifier's annotated
		// log, and that log only appears under %+v -- so formatting it into
		// the message here would produce an error that reads well once and
		// can never be inspected by a caller. Explain does the rendering at
		// the point where somebody is actually going to read it.
		return nil, fmt.Errorf("load %s: %w%s", path, err, hint)
	}

	loaded := &Loaded{coll: coll}
	for name, prog := range coll.Programs {
		loaded.Programs = append(loaded.Programs, Program{
			Name:         name,
			Type:         prog.Type(),
			Instructions: len(spec.Programs[name].Instructions),
			VerifierLog:  prog.VerifierLog,
		})
	}
	return loaded, nil
}

// Explain renders an error from Object for a human to read.
//
// It exists because a verifier rejection is not one line. The kernel returns
// the instruction it gave up on and the register state that made it give up,
// and Go prints that only under %+v -- so an error logged the ordinary way
// says "permission denied" or "invalid argument" and throws away the one
// thing that would have explained it.
func Explain(err error) string {
	if err == nil {
		return ""
	}
	var ve *ebpf.VerifierError
	if errors.As(err, &ve) {
		return fmt.Sprintf("%+v", ve)
	}
	return err.Error()
}

// Program returns a loaded program by the name of its C function.
func (l *Loaded) Program(name string) (*ebpf.Program, error) {
	prog, ok := l.coll.Programs[name]
	if !ok {
		return nil, fmt.Errorf("no program named %q in this object", name)
	}
	return prog, nil
}

// Close releases every program and map in the collection.
func (l *Loaded) Close() error {
	if l == nil || l.coll == nil {
		return nil
	}
	l.coll.Close()
	l.coll = nil
	return nil
}

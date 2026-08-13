package bpfload_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"

	"github.com/FranciscoPLoureiro/wallclock/internal/bpfload"
	"github.com/FranciscoPLoureiro/wallclock/internal/kerneltest"
)

// objectPathEnv lets CI point the test at an object built elsewhere in the
// workspace rather than assuming the layout of the checkout.
const objectPathEnv = "WALLCLOCK_BPF_OBJECT"

func objectPath(t *testing.T) string {
	t.Helper()
	if p := os.Getenv(objectPathEnv); p != "" {
		return p
	}
	return filepath.Join("..", "..", "build", "minimal.bpf.o")
}

// The proof of phase 0: clang produced an object, the kernel's verifier
// accepted it, and the tracepoint machinery attached it. Compiling proves
// none of those three.
func TestObjectLoadsAndAttaches(t *testing.T) {
	kerneltest.Root(t)

	path := objectPath(t)
	if _, err := os.Stat(path); err != nil {
		// Not a skip: a missing object means the build step did not run, and
		// that is a broken pipeline rather than an unsupported host.
		t.Fatalf("%s is not there; run `make bpf` first (%v)", path, err)
	}

	loaded, err := bpfload.Object(path)
	if err != nil {
		t.Fatalf("loading %s: %v", path, err)
	}
	defer loaded.Close()

	if len(loaded.Programs) != 1 {
		t.Fatalf("loaded %d programs, want exactly 1: %+v", len(loaded.Programs), loaded.Programs)
	}
	prog := loaded.Programs[0]

	if prog.Name != "wallclock_minimal" {
		t.Errorf("program name = %q, want wallclock_minimal", prog.Name)
	}
	if prog.Type != ebpf.TracePoint {
		t.Errorf("program type = %v, want TracePoint", prog.Type)
	}
	// Two: the return value and the exit. If clang ever emits more for a
	// function this small, something changed in the toolchain and the rest of
	// the phase-0 reasoning about verifier limits deserves rereading.
	if prog.Instructions != 2 {
		t.Errorf("program is %d instructions, want 2", prog.Instructions)
	}
	// The kernel's own words. Asserting on them is what makes this a proof
	// that the verifier ran, rather than a proof that the library returned
	// no error.
	if !strings.Contains(prog.VerifierLog, "processed 2 insns") {
		t.Errorf("verifier log does not show the program being processed: %q", prog.VerifierLog)
	}
	t.Logf("verifier: %s", strings.TrimSpace(prog.VerifierLog))

	// Loading needs CAP_BPF; attaching a tracepoint additionally needs
	// CAP_PERFMON, and every program this project will grow attaches this
	// way. Proving only the load would leave half the requirement untested.
	handle, err := loaded.Program(prog.Name)
	if err != nil {
		t.Fatalf("looking up the program just loaded: %v", err)
	}
	tp, err := link.Tracepoint("syscalls", "sys_enter_getpid", handle, nil)
	if err != nil {
		t.Fatalf("attaching to syscalls/sys_enter_getpid: %v", err)
	}
	if err := tp.Close(); err != nil {
		t.Errorf("detaching: %v", err)
	}
}

func TestObjectRejectsSomethingThatIsNotAnELF(t *testing.T) {
	// No kernel access needed: this fails while reading the file, which is
	// the point -- a bad path or a stale artifact should report as a read
	// error naming the file, not as an obscure verifier message.
	path := filepath.Join(t.TempDir(), "not-an-object.o")
	if err := os.WriteFile(path, []byte("this is not an ELF file\n"), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	loaded, err := bpfload.Object(path)
	if err == nil {
		loaded.Close()
		t.Fatal("a text file loaded as a BPF object")
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("error %q does not name the file it failed on", err)
	}
}

func TestProgramLookupNamesTheMissingProgram(t *testing.T) {
	kerneltest.Root(t)

	path := objectPath(t)
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("%s is not there; run `make bpf` first (%v)", path, err)
	}

	loaded, err := bpfload.Object(path)
	if err != nil {
		t.Fatalf("loading %s: %v", path, err)
	}
	defer loaded.Close()

	if _, err := loaded.Program("no_such_program"); err == nil {
		t.Error("looking up a program that is not in the object returned no error")
	} else if !strings.Contains(err.Error(), "no_such_program") {
		t.Errorf("error %q does not name the program that was asked for", err)
	}
}

// The verifier is the thing that will cost the most time on this project, so
// the record of what it refuses and why is a deliverable rather than a note.
//
// testdata/unchecked_lookup.bpf.c is the first program it refused here: a map
// lookup dereferenced without checking for NULL, which is ordinary C and
// unacceptable kernel code. Keeping it as a fixture means the claim in the
// README is re-checked on every run instead of aging quietly into fiction.
func TestVerifierRefusesAnUncheckedMapLookup(t *testing.T) {
	kerneltest.Root(t)

	path := filepath.Join("testdata", "unchecked_lookup.bpf.o")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("%s is not there; run `make bpf` first (%v)", path, err)
	}

	loaded, err := bpfload.Object(path)
	if err == nil {
		loaded.Close()
		t.Fatal("the kernel accepted a program that dereferences a map lookup " +
			"without checking it for NULL")
	}

	// Explain is what turns this from "invalid argument" into the verifier's
	// account of which instruction it gave up on. Asserting on the text is
	// asserting on the reason, not merely on the failure: a program rejected
	// for running out of instructions would otherwise pass this test.
	explained := bpfload.Explain(err)
	if !strings.Contains(explained, "map_value_or_null") {
		t.Errorf("the rejection does not mention the unchecked pointer:\n%s", explained)
	}
	t.Logf("verifier said:\n%s", explained)
}

// Close is called from defer in every test above and from the CLI, so it has
// to tolerate being called on a nil receiver and twice on the same value.
func TestCloseIsSafeWhenThereIsNothingToClose(t *testing.T) {
	var nilLoaded *bpfload.Loaded
	if err := nilLoaded.Close(); err != nil {
		t.Errorf("closing a nil Loaded: %v", err)
	}
}

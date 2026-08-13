package bpfload_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"

	"github.com/FranciscoPLoureiro/wallclock/internal/bpfload"
)

// requireBPFEnv turns the skip on an unprivileged machine into a failure when
// it is set. Loading a program needs root, so this test skips on a developer
// laptop -- and a test that skips in CI proves exactly as much as no test at
// all, while still colouring the run green. CI sets this, so a runner that
// cannot load BPF fails loudly instead of quietly skipping the one check the
// whole pipeline exists for.
const requireBPFEnv = "WALLCLOCK_REQUIRE_BPF"

// objectPathEnv lets CI point the test at an object built elsewhere in the
// workspace rather than assuming the layout of the checkout.
const objectPathEnv = "WALLCLOCK_BPF_OBJECT"

func requireKernelAccess(t *testing.T) {
	t.Helper()
	if os.Geteuid() == 0 {
		return
	}
	if os.Getenv(requireBPFEnv) == "1" {
		t.Fatalf("%s=1 but euid is %d: the kernel load would have been skipped, "+
			"which is the failure this variable exists to prevent",
			requireBPFEnv, os.Geteuid())
	}
	t.Skipf("not root, so nothing can be loaded; set %s=1 to make this a failure", requireBPFEnv)
}

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
	requireKernelAccess(t)

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
	requireKernelAccess(t)

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

// Close is called from defer in every test above and from the CLI, so it has
// to tolerate being called on a nil receiver and twice on the same value.
func TestCloseIsSafeWhenThereIsNothingToClose(t *testing.T) {
	var nilLoaded *bpfload.Loaded
	if err := nilLoaded.Close(); err != nil {
		t.Errorf("closing a nil Loaded: %v", err)
	}
}

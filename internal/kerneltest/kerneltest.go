// Package kerneltest holds the guard shared by every test that needs a real
// kernel to prove anything.
//
// It is a package rather than a copied helper because the rule it encodes is
// the one thing these tests must not get wrong: skipping is acceptable on a
// developer machine and unacceptable in CI, and a rule expressed twice is a
// rule that will eventually be expressed differently.
package kerneltest

import (
	"os"
	"testing"
)

// RequireEnv, when set to "1", turns the unprivileged skip into a failure.
// CI sets it. Without it a runner that could not load a single program would
// report a green tick over a suite that did nothing, which is worse than
// having no suite because it also stops anyone looking.
const RequireEnv = "WALLCLOCK_REQUIRE_BPF"

// Root skips the calling test unless it can load BPF programs, or fails it if
// RequireEnv says skipping is not allowed here.
func Root(t *testing.T) {
	t.Helper()
	if os.Geteuid() == 0 {
		return
	}
	if os.Getenv(RequireEnv) == "1" {
		t.Fatalf("%s=1 but euid is %d: the kernel work would have been skipped, "+
			"which is the failure this variable exists to prevent",
			RequireEnv, os.Geteuid())
	}
	t.Skipf("not root, so nothing can be loaded; set %s=1 to make this a failure", RequireEnv)
}

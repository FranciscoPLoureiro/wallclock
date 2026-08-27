// Package spin resolves the shell that the CPU-burning subjects are copied
// from.
//
// The subjects in the tests and in `wallclock validate` are copies of a shell
// binary under a distinctive name, because comm comes from the executable's
// name and the kernel does not read argv[0] for it. Which shell that is
// varies by distribution: Debian and Ubuntu ship /bin/dash, Arch and Fedora
// do not. Hardcoding it turned the throttling test -- the one claim this
// project exists to make -- into a silent skip on every non-Debian host, and
// made `wallclock validate` refuse to start there for a reason that had
// nothing to do with the kernel it was validating against.
//
// Failing to find a shell is an error rather than a reason to skip. /bin/sh
// is required by POSIX and is present on every userland this can run under,
// so a host without one is broken rather than merely different -- and a skip
// is how the original defect stayed invisible.
package spin

import (
	"fmt"
	"os"
)

// Tried in order. dash first where it exists, because it is the smallest and
// starts fastest; /bin/sh after it, because it is the one POSIX guarantees
// and is whatever the distribution points it at. Every candidate is a shell
// that accepts `-c`, which busybox -- deliberately absent -- does not.
var candidates = []string{
	"/bin/dash",
	"/bin/sh",
	"/bin/bash",
	"/usr/bin/dash",
	"/usr/bin/sh",
	"/usr/bin/bash",
}

// Shell returns the contents of the first shell found and the path it came
// from. The bytes rather than the path, because every caller writes them out
// under a new name and the read is the only thing any of them does with it.
func Shell() ([]byte, string, error) {
	for _, path := range candidates {
		body, err := os.ReadFile(path)
		if err == nil {
			return body, path, nil
		}
	}
	return nil, "", fmt.Errorf("no shell to copy as the spinner: tried %v", candidates)
}

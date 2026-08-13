package preflight

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseRelease(t *testing.T) {
	// The release strings are real ones: WSL2, Debian stable, an Ubuntu
	// cloud image, a release candidate, and a kernel built without a
	// sublevel. The parser is trivial and the input is not, which is the
	// whole reason it is a function with a test rather than a Split call at
	// the call site.
	cases := []struct {
		name    string
		release string
		major   int
		minor   int
		wantErr bool
	}{
		{name: "wsl2", release: "6.6.87.2-microsoft-standard-WSL2", major: 6, minor: 6},
		{name: "debian", release: "5.10.0-28-amd64", major: 5, minor: 10},
		{name: "azure", release: "5.15.0-1050-azure", major: 5, minor: 15},
		{name: "release candidate", release: "6.12.0-rc3+", major: 6, minor: 12},
		{name: "no sublevel", release: "6.1", major: 6, minor: 1},
		{name: "major only", release: "6", major: 6, minor: 0},
		{name: "minor glued to suffix", release: "5.8rc1.foo", major: 5, minor: 8},
		{name: "empty", release: "", wantErr: true},
		{name: "not a version", release: "custom-kernel", wantErr: true},
		{name: "empty minor", release: "6..3", wantErr: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			major, minor, err := parseRelease(tc.release)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("parseRelease(%q) = %d.%d, want an error", tc.release, major, minor)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseRelease(%q): %v", tc.release, err)
			}
			if major != tc.major || minor != tc.minor {
				t.Errorf("parseRelease(%q) = %d.%d, want %d.%d",
					tc.release, major, minor, tc.major, tc.minor)
			}
		})
	}
}

// The cpu controller check reads a file that only exists on a cgroup v2 host,
// so the failure paths are the ones worth proving: they are what a developer
// on the wrong kernel actually hits, and they are unreachable on a machine
// where everything works.
func TestCheckCPUController(t *testing.T) {
	cases := []struct {
		name        string
		controllers string // contents of cgroup.controllers, "" means do not create it
		wantOK      bool
		wantDetail  string
	}{
		{
			name:        "cpu enabled",
			controllers: "cpuset cpu io memory hugetlb pids rdma\n",
			wantOK:      true,
			wantDetail:  "enabled at the root",
		},
		{
			name: "cpu not delegated",
			// A cgroup whose parent never wrote cpu into subtree_control sees
			// exactly this: a hierarchy that exists, without the controller
			// that carries cpu.max and cpu.stat.
			controllers: "memory pids\n",
			wantOK:      false,
			wantDetail:  "cpu absent",
		},
		{
			name:        "no unified hierarchy",
			controllers: "",
			wantOK:      false,
			wantDetail:  "read",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			if tc.controllers != "" {
				path := filepath.Join(root, "cgroup.controllers")
				if err := os.WriteFile(path, []byte(tc.controllers), 0o600); err != nil {
					t.Fatalf("write fixture: %v", err)
				}
			}

			got := checkCPUController(host{cgroupRoot: root})
			if got.OK != tc.wantOK {
				t.Errorf("OK = %v, want %v (detail: %s)", got.OK, tc.wantOK, got.Detail)
			}
			if !strings.Contains(got.Detail, tc.wantDetail) {
				t.Errorf("Detail = %q, want it to contain %q", got.Detail, tc.wantDetail)
			}
		})
	}
}

func TestCheckCgroupV2RejectsOtherFilesystems(t *testing.T) {
	// t.TempDir is on whatever filesystem the test runs from, which is never
	// cgroup2. That makes it a free negative case for the magic-number check:
	// a directory that exists and is readable, and still is not the unified
	// hierarchy.
	got := checkCgroupV2(host{cgroupRoot: t.TempDir()})
	if got.OK {
		t.Errorf("a temp directory passed the cgroup2 check: %s", got.Detail)
	}
	if !strings.Contains(got.Detail, "not cgroup2") {
		t.Errorf("Detail = %q, want it to say the filesystem is not cgroup2", got.Detail)
	}
}

func TestCheckCgroupV2ReportsMissingMountpoint(t *testing.T) {
	got := checkCgroupV2(host{cgroupRoot: filepath.Join(t.TempDir(), "absent")})
	if got.OK {
		t.Error("a path that does not exist passed the cgroup2 check")
	}
	if !strings.Contains(got.Detail, "statfs") {
		t.Errorf("Detail = %q, want it to name the failing syscall", got.Detail)
	}
}

// Every check is also a row of docs/COMPATIBILITY.md. A check with no
// consequence would render as a requirement nobody can act on, and a check
// with no detail hides the value that failed -- both are the kind of gap that
// only shows up on the host where the check fails, which is the host least
// able to debug it.
func TestEveryCheckExplainsItself(t *testing.T) {
	report := Run()
	if len(report.Checks) == 0 {
		t.Fatal("Run returned no checks")
	}

	for _, c := range report.Checks {
		if c.Name == "" {
			t.Error("a check has no name")
		}
		if c.Detail == "" {
			t.Errorf("check %q reports no detail", c.Name)
		}
		if c.Consequence == "" {
			t.Errorf("check %q does not say what breaks without it", c.Name)
		}
	}
}

func TestReportOKAndFailed(t *testing.T) {
	report := Report{Checks: []Check{
		{Name: "first", OK: true},
		{Name: "second", OK: false},
		{Name: "third", OK: true},
	}}

	if report.OK() {
		t.Error("OK() is true for a report containing a failure")
	}
	failed := report.Failed()
	if len(failed) != 1 || failed[0].Name != "second" {
		t.Errorf("Failed() = %+v, want just the second check", failed)
	}

	allGood := Report{Checks: []Check{{Name: "only", OK: true}}}
	if !allGood.OK() {
		t.Error("OK() is false for a report with no failures")
	}
	if got := allGood.Failed(); got != nil {
		t.Errorf("Failed() = %+v, want nil", got)
	}
}

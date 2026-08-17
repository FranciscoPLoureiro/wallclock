package netlat_test

import (
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/FranciscoPLoureiro/wallclock/internal/netlat"
)

func writeNameFile(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "names")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write the name file: %v", err)
	}
	return path
}

func dest(t *testing.T, addr string, port uint16) netlat.Destination {
	t.Helper()
	a, err := netip.ParseAddr(addr)
	if err != nil {
		t.Fatalf("parse %q: %v", addr, err)
	}
	return netlat.Destination{Addr: a, Port: port}
}

func TestNamesPreferTheMoreSpecificEntry(t *testing.T) {
	path := writeNameFile(t, `
# the whole host, and then one service on it
10.5.0.4        database-host
10.5.0.4:5432   postgres

10.5.0.3:6379   redis
`)
	names, err := netlat.LoadNames(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	for _, c := range []struct {
		d    netlat.Destination
		want string
	}{
		{dest(t, "10.5.0.4", 5432), "postgres"},      // the port entry wins
		{dest(t, "10.5.0.4", 9187), "database-host"}, // and the host entry catches the rest
		{dest(t, "10.5.0.3", 6379), "redis"},
		{dest(t, "10.5.0.9", 80), ""}, // unknown stays unnamed rather than guessed
	} {
		if got := names.Lookup(c.d); got != c.want {
			t.Errorf("%s named %q, want %q", c.d, got, c.want)
		}
	}
}

// A nil table is the ordinary case -- no -names flag was given -- and must
// not be a special case at every call site.
func TestNilNamesLookUpToNothing(t *testing.T) {
	var names *netlat.Names
	if got := names.Lookup(dest(t, "10.0.0.1", 80)); got != "" {
		t.Errorf("nil table returned %q", got)
	}
}

func TestNameFileErrorsSayWhichLine(t *testing.T) {
	for _, c := range []struct {
		name, body, wantIn string
	}{
		{"three fields", "10.0.0.1 a b\n", ":1:"},
		{"not an address", "not-a-host redis\n", ":1:"},
		{"bad line after good ones", "10.0.0.1 a\n\n10.0.0.2 b\nnonsense\n", ":4:"},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := netlat.LoadNames(writeNameFile(t, c.body))
			if err == nil {
				t.Fatal("accepted a malformed name file")
			}
			if !strings.Contains(err.Error(), c.wantIn) {
				t.Errorf("error %q does not point at %s", err, c.wantIn)
			}
		})
	}
}

// A missing file is an error, not an empty table: the flag was passed on
// purpose, and naming nothing in silence is the most annoying way to fail.
func TestAMissingNameFileIsAnError(t *testing.T) {
	if _, err := netlat.LoadNames(filepath.Join(t.TempDir(), "absent")); err == nil {
		t.Error("a missing name file was accepted")
	}
}

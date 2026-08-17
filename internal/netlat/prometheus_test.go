package netlat_test

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/FranciscoPLoureiro/wallclock/internal/netlat"
)

// The buckets have to be cumulative, which is the part of the exposition
// format everybody gets wrong on the first attempt. Prometheus reads le="x"
// as "observations at most x", not "observations in this bucket", and a
// histogram written the other way produces quantiles that are silently
// nonsense rather than an error anybody sees.
func TestBucketsAreCumulativeAndEndAtInf(t *testing.T) {
	var s netlat.Stats
	s.Destination = dest(t, "10.5.0.3", 6379)
	s.Buckets[3] = 7  // 8-16us
	s.Buckets[10] = 2 // 1024-2048us
	s.Buckets[12] = 1 // 4096-8192us
	s.Count = 10
	s.Total = 12 * time.Millisecond

	var out strings.Builder
	if err := netlat.WriteMetrics(&out, []netlat.Stats{s}, nil); err != nil {
		t.Fatalf("write: %v", err)
	}
	text := out.String()

	// Walking up the buckets, the count must never fall.
	var previous uint64
	seen := 0
	for _, line := range strings.Split(text, "\n") {
		if !strings.Contains(line, "_bucket{") || strings.Contains(line, `le="+Inf"`) {
			continue
		}
		value := valueOf(t, line)
		if value < previous {
			t.Fatalf("bucket counts go down at %q: %d after %d", line, value, previous)
		}
		previous = value
		seen++
	}
	if seen != 128 {
		t.Errorf("wrote %d finite buckets, want 128", seen)
	}
	if previous != s.Count {
		t.Errorf("the last finite bucket holds %d of %d observations", previous, s.Count)
	}

	for _, want := range []string{
		`le="+Inf"} 10`,
		`_count{destination="10.5.0.3:6379"} 10`,
		`_sum{destination="10.5.0.3:6379"} 0.012`,
		"# TYPE wallclock_destination_latency_seconds histogram",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("output is missing %q\n%s", want, text)
		}
	}
}

// Seconds, not milliseconds: Prometheus base units are seconds, and a
// dashboard built on anything else silently disagrees with every other
// histogram on the machine.
func TestBucketEdgesAreInSeconds(t *testing.T) {
	var s netlat.Stats
	s.Destination = dest(t, "10.0.0.1", 80)
	s.Count = 1
	s.Buckets[0] = 1

	var out strings.Builder
	if err := netlat.WriteMetrics(&out, []netlat.Stats{s}, nil); err != nil {
		t.Fatalf("write: %v", err)
	}
	// The first log2 bucket ends at 2 microseconds, which is 2e-06 seconds.
	if !strings.Contains(out.String(), `le="2e-06"`) {
		t.Errorf("the first bucket edge is not 2 microseconds expressed in seconds:\n%s",
			out.String())
	}
}

func TestNamesBecomeALabelAndAreEscaped(t *testing.T) {
	path := writeNameFile(t, "10.5.0.3:6379 the\\\"cache\n")
	names, err := netlat.LoadNames(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	var s netlat.Stats
	s.Destination = dest(t, "10.5.0.3", 6379)
	s.Count = 1
	s.Buckets[0] = 1

	var out strings.Builder
	if err := netlat.WriteMetrics(&out, []netlat.Stats{s}, names); err != nil {
		t.Fatalf("write: %v", err)
	}
	// A quote in a name must not end the label value early, which would make
	// Prometheus reject the whole scrape rather than one line of it.
	if !strings.Contains(out.String(), `name="the\\\"cache"`) {
		t.Errorf("the quote in the name was not escaped:\n%s", out.String())
	}
}

// An unnamed destination gets no name label at all, rather than an empty one:
// an empty label is a real label value in Prometheus and would split a series
// in two for no reason.
func TestUnnamedDestinationsHaveNoNameLabel(t *testing.T) {
	var s netlat.Stats
	s.Destination = dest(t, "10.0.0.9", 443)
	s.Count = 1
	s.Buckets[0] = 1

	var out strings.Builder
	if err := netlat.WriteMetrics(&out, []netlat.Stats{s}, nil); err != nil {
		t.Fatalf("write: %v", err)
	}
	if strings.Contains(out.String(), "name=") {
		t.Errorf("an unnamed destination was given a name label:\n%s", out.String())
	}
}

func valueOf(t *testing.T, line string) uint64 {
	t.Helper()
	fields := strings.Fields(line)
	if len(fields) != 2 {
		t.Fatalf("cannot read a value from %q", line)
	}
	v, err := strconv.ParseUint(fields[1], 10, 64)
	if err != nil {
		t.Fatalf("parse %q: %v", fields[1], err)
	}
	return v
}

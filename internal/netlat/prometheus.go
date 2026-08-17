package netlat

import (
	"bufio"
	"fmt"
	"io"
	"strings"
)

// metricName is the series these histograms are published as.
const metricName = "wallclock_destination_latency_seconds"

// WriteMetrics writes the destinations as a Prometheus histogram, in the text
// exposition format.
//
// Written by hand rather than with the client library, and that is a
// deliberate trade. The library would bring in a dependency tree larger than
// this repository to produce forty lines of text whose format is stable, four
// pages long, and already fully determined by the data here. The cost is that
// the format has to be got right; the test asserts the parts that are easy to
// get wrong, which are the cumulative buckets and the +Inf.
//
// The buckets are cumulative, which is the part everybody gets wrong on the
// first try. Prometheus defines le="x" as "how many observations were at most
// x", not "how many fell in this bucket" -- so each one carries the sum of
// everything below it, and the last is le="+Inf" and equals the count.
//
// Seconds, because Prometheus base units are seconds and a dashboard built on
// milliseconds silently disagrees with every other histogram on the machine.
func WriteMetrics(w io.Writer, stats []Stats, names *Names) error {
	out := bufio.NewWriter(w)

	fmt.Fprintf(out, "# HELP %s Time from a request going out to the first bytes of its answer coming back.\n", metricName)
	fmt.Fprintf(out, "# TYPE %s histogram\n", metricName)

	for _, s := range stats {
		// Quoted by hand rather than with %q, which would escape the
		// already-escaped value a second time -- and which escapes more than
		// this format defines anyway. Prometheus says a label value escapes
		// a backslash, a double quote and a newline; Go's %q also turns a
		// tab into \t and anything non-ASCII into \u, neither of which a
		// scraper is obliged to understand.
		labels := fmt.Sprintf(`destination="%s"`, escapeLabel(s.Destination.String()))
		if name := names.Lookup(s.Destination); name != "" {
			labels += fmt.Sprintf(`,name="%s"`, escapeLabel(name))
		}

		var cumulative uint64
		for i, n := range s.Buckets {
			cumulative += n
			edge := bucketCeiling(i).Seconds()
			fmt.Fprintf(out, "%s_bucket{%s,le=\"%g\"} %d\n", metricName, labels, edge, cumulative)
		}
		fmt.Fprintf(out, "%s_bucket{%s,le=\"+Inf\"} %d\n", metricName, labels, s.Count)
		fmt.Fprintf(out, "%s_sum{%s} %g\n", metricName, labels, s.Total.Seconds())
		fmt.Fprintf(out, "%s_count{%s} %d\n", metricName, labels, s.Count)
	}
	return out.Flush()
}

// escapeLabel makes a name safe to put between quotes in the exposition
// format, where a backslash, a quote or a newline would otherwise end the
// value early and produce a file Prometheus rejects wholesale.
func escapeLabel(s string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(s)
}

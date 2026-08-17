// Package netlat measures how long each destination takes to answer.
//
// "Blocked on network 22%" is half an answer; the other half is whose
// network. The original plan for this was to remember which destination each
// thread last spoke to and charge its blocked time there when it stopped.
// Phase 3 measured what that produces on a modern stack and the answer was
// almost nothing: a Go client parks goroutines in a netpoller and stops no
// thread, and PostgreSQL and Redis wait in epoll. Over a whole machine under
// load, six threads recorded any network-blocked time at all -- 164 ms
// between them, all short-lived one-shot clients, not one of them a service.
//
// So the interval measured here does not require a thread to block: it is the
// time from a thread sending on a connection to the first bytes coming back
// on that same connection. That exists whether the waiting was done by a
// blocked thread, a parked goroutine or an event loop, because it is a
// property of the conversation rather than of the scheduler.
package netlat

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net/netip"
	"sort"
	"time"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"
	"github.com/cilium/ebpf/rlimit"
)

//go:generate go tool bpf2go -target bpfel -type destination -type dest_stats -type stat_slot netlat ../../bpf/netlat.bpf.c -- -D__TARGET_ARCH_x86 -I/usr/include/x86_64-linux-gnu

// These mirror the histogram shape in the BPF program: four buckets per
// octave, over thirty-two octaves of microseconds.
const (
	slots      = 128
	subBits    = 2
	subBuckets = 1 << subBits
)

// bucketCeiling is the largest duration that lands in a bucket.
//
// Below subBuckets microseconds each value has a bucket to itself. Above it a
// bucket covers a quarter of an octave: the values in octave o whose top two
// mantissa bits are s run up to (subBuckets + s + 1) << (o - subBits).
func bucketCeiling(slot int) time.Duration {
	if slot < subBuckets {
		return time.Duration(slot+1) * time.Microsecond
	}
	octave := slot / subBuckets
	sub := slot % subBuckets
	if octave < subBits {
		// Unreachable -- anything in octave 0 or 1 was handled above -- but
		// arithmetic that would otherwise shift by a negative amount should
		// not depend on that staying true.
		return time.Duration(slot+1) * time.Microsecond
	}
	// The last slot is 127: octave 31, a shift of 29, so a ceiling of about
	// 4295 seconds expressed in microseconds. Nowhere near overflowing a
	// signed nanosecond Duration, which runs to 292 years.
	return time.Duration(uint64(subBuckets+sub+1)<<(octave-subBits)) * time.Microsecond //nolint:gosec // see above
}

// Destination is the far end of a connection.
type Destination struct {
	Addr netip.Addr
	Port uint16
}

func (d Destination) String() string {
	return netip.AddrPortFrom(d.Addr, d.Port).String()
}

// Stats is what one destination did over the observed window.
type Stats struct {
	Destination Destination
	// Count is how many request-and-answer pairs were completed.
	Count uint64
	Total time.Duration
	Max   time.Duration
	// Buckets is a histogram of microseconds with four buckets to the
	// octave. bucketCeiling says what each one covers.
	Buckets [slots]uint64
}

// Mean is the average round trip to this destination.
func (s Stats) Mean() time.Duration {
	if s.Count == 0 {
		return 0
	}
	return s.Total / time.Duration(s.Count) //nolint:gosec // Count is positive here
}

// Percentile reads a quantile off the histogram, returning the upper bound of
// the bucket the sample falls in.
//
// An upper bound and not an interpolation, and the difference matters when
// somebody quotes the number. The histogram knows only which bucket a sample
// landed in; interpolating inside it would invent precision the data does not
// have. So a p99 of "4ms" here means "at or below 4 ms", which is what the
// measurement supports. Four buckets to the octave bounds that overstatement
// at 25%.
func (s Stats) Percentile(q float64) time.Duration {
	if s.Count == 0 {
		return 0
	}
	target := uint64(q * float64(s.Count))
	if target == 0 {
		target = 1
	}
	var seen uint64
	for i, n := range s.Buckets {
		seen += n
		if seen >= target {
			return bucketCeiling(i)
		}
	}
	return s.Max
}

// Drops is what the kernel side could not record.
type Drops struct {
	// DestinationsFull counts exchanges whose destination could not be
	// added because that map was at max_entries. Their latency is missing
	// from the report entirely rather than misfiled.
	DestinationsFull uint64
	// ThreadsFull counts threads whose in-flight request could not be
	// recorded, so their next answer has nothing to pair with.
	ThreadsFull uint64
	// Unpaired counts answers that arrived with no matching question from
	// the same thread to the same peer. This is *not* a loss: a server
	// reading a request has no outstanding send, and neither does the
	// second half of a reply whose first half already closed the interval.
	// It is reported because a large number here means the traffic is not
	// request-and-answer shaped, and then the latencies mean something
	// other than what a reader will assume.
	Unpaired uint64
	// SocketsFull counts requests whose connection could not be recorded
	// because that map was at max_entries, so their answers pair with
	// nothing.
	SocketsFull uint64
}

// Any reports whether anything was actually lost. Unpaired is excluded: it is
// a description of the traffic, not a failure to record it.
func (d Drops) Any() bool {
	return d.DestinationsFull > 0 || d.ThreadsFull > 0 || d.SocketsFull > 0
}

// Session owns the loaded programs, their maps and the attachments.
type Session struct {
	objs  netlatObjects
	links []link.Link
}

// Open attaches to the TCP send and receive paths.
//
// A zero cgroup watches the whole machine. Naming one narrows it in the
// kernel to a single comparison per event -- and a cgroup id is the filter
// that means the same thing inside a container and out, which a pid is not.
func Open(cgroupID uint64) (_ *Session, err error) {
	if err := rlimit.RemoveMemlock(); err != nil {
		return nil, fmt.Errorf("raise RLIMIT_MEMLOCK: %w", err)
	}

	spec, err := loadNetlat()
	if err != nil {
		return nil, fmt.Errorf("read the embedded object: %w", err)
	}
	if err := spec.Variables["target_cgroup"].Set(cgroupID); err != nil {
		return nil, fmt.Errorf("set target_cgroup: %w", err)
	}

	s := &Session{}
	if err := spec.LoadAndAssign(&s.objs, nil); err != nil {
		return nil, fmt.Errorf("load: %w", err)
	}
	defer func() {
		if err != nil {
			_ = s.Close()
		}
	}()

	// The return probe goes on before the entry probes, and the order is a
	// dependency order rather than a preference: an entry probe attached
	// first would record a send that nothing is yet watching for an answer
	// to, and that thread's first exchange would be counted as unpaired.
	// It is a window of microseconds and it is free to close.
	for _, probe := range []struct {
		symbol  string
		ret     bool
		program *ebpf.Program
	}{
		{"tcp_recvmsg", true, s.objs.OnTcpRecvmsgRet},
		{"tcp_recvmsg", false, s.objs.OnTcpRecvmsg},
		{"tcp_sendmsg", false, s.objs.OnTcpSendmsg},
		{"tcp_close", false, s.objs.OnTcpClose},
	} {
		var l link.Link
		var attachErr error
		if probe.ret {
			l, attachErr = link.Kretprobe(probe.symbol, probe.program, nil)
		} else {
			l, attachErr = link.Kprobe(probe.symbol, probe.program, nil)
		}
		if attachErr != nil {
			kind := "kprobe"
			if probe.ret {
				kind = "kretprobe"
			}
			err = fmt.Errorf("attach a %s to %s: %w", kind, probe.symbol, attachErr)
			return nil, err
		}
		s.links = append(s.links, l)
	}
	return s, nil
}

// Destinations reads what every destination did, slowest mean first.
func (s *Session) Destinations() ([]Stats, error) {
	var (
		out []Stats
		key netlatDestination
		raw netlatDestStats
	)
	iter := s.objs.Destinations.Iterate()
	for iter.Next(&key, &raw) {
		stats := Stats{
			Destination: decodeDestination(key),
			Count:       raw.Count,
			Total:       time.Duration(raw.TotalNs), //nolint:gosec // ns, signed after 292 years
			Max:         time.Duration(raw.MaxNs),   //nolint:gosec // as above
		}
		copy(stats.Buckets[:], raw.Slots[:])
		out = append(out, stats)
	}
	if err := iter.Err(); err != nil {
		return nil, fmt.Errorf("iterate the destinations map: %w", err)
	}

	// Slowest first, because the reason anybody runs this is to find the
	// slow one. Ties broken by address so two runs over the same traffic
	// do not disagree about the order.
	sort.Slice(out, func(i, j int) bool {
		if out[i].Mean() != out[j].Mean() {
			return out[i].Mean() > out[j].Mean()
		}
		return out[i].Destination.String() < out[j].Destination.String()
	})
	return out, nil
}

// decodeDestination turns the kernel's view of a peer into an address and a
// port, and the byte order here is subtle enough to have been wrong once.
//
// The kernel holds skc_daddr and skc_dport in *network* order. The generated
// Go types are a plain uint32 and uint16, decoded from the map with the
// host's native order -- so on a little-endian machine the numeric value is
// the network bytes read backwards, and it is meaningless as a number.
//
// Writing it back out in native order recovers exactly the bytes the kernel
// had. Those bytes are the address, in the order an address is written, so
// netip takes them directly; and for the port they are a big-endian integer,
// because that is what the wire uses. The first version of this reversed them
// again with BigEndian.PutUint32 and reported 127.0.0.1 as 1.0.0.127 -- which
// is a plausible-looking address, in a report full of addresses, and the
// reason this note is longer than the code.
func decodeDestination(key netlatDestination) Destination {
	var addr [4]byte
	binary.NativeEndian.PutUint32(addr[:], key.Daddr)

	var port [2]byte
	binary.NativeEndian.PutUint16(port[:], key.Dport)

	return Destination{
		Addr: netip.AddrFrom4(addr),
		Port: binary.BigEndian.Uint16(port[:]),
	}
}

// Drops reports what the kernel side could not record.
func (s *Session) Drops() (Drops, error) {
	var d Drops
	for _, slot := range []struct {
		index netlatStatSlot
		into  *uint64
	}{
		{netlatStatSlotSTAT_DESTINATIONS_FULL, &d.DestinationsFull},
		{netlatStatSlotSTAT_THREADS_FULL, &d.ThreadsFull},
		{netlatStatSlotSTAT_UNPAIRED, &d.Unpaired},
		{netlatStatSlotSTAT_SOCKETS_FULL, &d.SocketsFull},
	} {
		if err := s.objs.Stats.Lookup(uint32(slot.index), slot.into); err != nil {
			return Drops{}, fmt.Errorf("read drop counter %d: %w", slot.index, err)
		}
	}
	return d, nil
}

// Close detaches every program and releases the maps.
func (s *Session) Close() error {
	var errs []error
	for _, l := range s.links {
		errs = append(errs, l.Close())
	}
	s.links = nil
	errs = append(errs, s.objs.Close())
	return errors.Join(errs...)
}

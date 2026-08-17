package netlat

import (
	"bufio"
	"fmt"
	"net/netip"
	"os"
	"strings"
)

// Names turns addresses into something a person recognises.
//
// Read from a file the user writes, and deliberately nothing more. Service
// discovery was considered and refused: guessing that 10.5.0.3:6379 is Redis
// because of the port number is right often enough to be trusted and wrong
// often enough to mislead, and a tool whose job is to say where the time went
// cannot afford a label that is usually correct. Where no name is given the
// report shows the address, which is always true.
//
// Two forms, and the more specific wins:
//
//	10.5.0.3:6379   redis        one service on one port
//	10.5.0.4        postgres     every port on one host
//
// Blank lines and everything after a # are ignored.
type Names struct {
	byAddrPort map[netip.AddrPort]string
	byAddr     map[netip.Addr]string
}

// LoadNames reads a name file. A missing path is an error rather than an
// empty table: the flag was passed on purpose, and silently naming nothing is
// the failure this would be most annoying to debug.
func LoadNames(path string) (*Names, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open the name file: %w", err)
	}
	defer file.Close()

	names := &Names{
		byAddrPort: make(map[netip.AddrPort]string),
		byAddr:     make(map[netip.Addr]string),
	}
	scanner := bufio.NewScanner(file)
	for line := 1; scanner.Scan(); line++ {
		text := scanner.Text()
		if hash := strings.IndexByte(text, '#'); hash >= 0 {
			text = text[:hash]
		}
		fields := strings.Fields(text)
		if len(fields) == 0 {
			continue
		}
		if len(fields) != 2 {
			return nil, fmt.Errorf("%s:%d: want an address and a name, got %d fields",
				path, line, len(fields))
		}

		target, name := fields[0], fields[1]
		if addrPort, err := netip.ParseAddrPort(target); err == nil {
			names.byAddrPort[addrPort] = name
			continue
		}
		addr, err := netip.ParseAddr(target)
		if err != nil {
			return nil, fmt.Errorf("%s:%d: %q is neither an address nor an "+
				"address:port", path, line, target)
		}
		names.byAddr[addr] = name
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read the name file: %w", err)
	}
	return names, nil
}

// Lookup returns the name for a destination, or "" if it has none. The
// address-and-port entry wins over the address-only one, so a host can be
// named in general and one of its ports named specifically.
func (n *Names) Lookup(d Destination) string {
	if n == nil {
		return ""
	}
	if name, ok := n.byAddrPort[netip.AddrPortFrom(d.Addr, d.Port)]; ok {
		return name
	}
	return n.byAddr[d.Addr]
}

// Package ksyms turns kernel addresses into function names.
//
// BPF cannot do this: a program can capture a stack of addresses and has no
// way to name them, because the symbol table is not something the kernel
// exposes to it. So the addresses come up whole and are resolved here, once,
// against /proc/kallsyms.
package ksyms

import (
	"bufio"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
)

// Table maps addresses to the function containing them.
type Table struct {
	// Parallel arrays rather than a slice of structs: the lookup is a binary
	// search over addresses and nothing else, and this keeps that search
	// reading one contiguous run of memory.
	addrs []uint64
	names []string
}

// Load reads /proc/kallsyms.
//
// It must be read as root, and the failure when it is not is silent by
// design: with kptr_restrict set, every address reads as zero rather than the
// file refusing to open. That is checked here, because the alternative is a
// flame graph of a thousand frames all called the same thing.
func Load() (*Table, error) {
	file, err := os.Open("/proc/kallsyms")
	if err != nil {
		return nil, fmt.Errorf("open /proc/kallsyms: %w", err)
	}
	defer file.Close()

	t := &Table{}
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		// "ffffffff81000000 T _text", with an optional module in brackets.
		fields := strings.Fields(scanner.Text())
		if len(fields) < 3 {
			continue
		}
		addr, err := strconv.ParseUint(fields[0], 16, 64)
		if err != nil {
			continue
		}
		t.addrs = append(t.addrs, addr)
		t.names = append(t.names, fields[2])
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read /proc/kallsyms: %w", err)
	}
	if len(t.addrs) == 0 {
		return nil, fmt.Errorf("/proc/kallsyms held no symbols")
	}

	sort.Sort(t)

	// Every address zero means kptr_restrict hid them, which resolves every
	// frame in every stack to the same name and produces a report that looks
	// complete and says nothing.
	if t.addrs[len(t.addrs)-1] == 0 {
		return nil, fmt.Errorf(
			"/proc/kallsyms reports every symbol at address zero, which means " +
				"kernel pointers are restricted -- run as root")
	}
	return t, nil
}

func (t *Table) Len() int           { return len(t.addrs) }
func (t *Table) Less(i, j int) bool { return t.addrs[i] < t.addrs[j] }
func (t *Table) Swap(i, j int) {
	t.addrs[i], t.addrs[j] = t.addrs[j], t.addrs[i]
	t.names[i], t.names[j] = t.names[j], t.names[i]
}

// Resolve names the function containing an address.
//
// An address between two symbols belongs to the earlier one, which is what
// makes a return address in the middle of a function resolve to that
// function. Anything below the first symbol has no name and is returned as
// hex, so a stack with an unresolvable frame still reads as a stack rather
// than losing a level.
func (t *Table) Resolve(addr uint64) string {
	if addr == 0 {
		return ""
	}
	i := sort.Search(len(t.addrs), func(i int) bool { return t.addrs[i] > addr })
	if i == 0 {
		return fmt.Sprintf("0x%x", addr)
	}
	return t.names[i-1]
}

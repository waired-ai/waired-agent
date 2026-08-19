package hardware

import (
	"errors"
	"strconv"
	"strings"
)

// This file holds the pure half of the macOS availability probe. The
// darwin-tagged file holds only the exec, which is the seam rule
// CLAUDE.md §Test discipline asks for and that appleGPUModel
// (gpu_apple.go) already follows: "Untagged and pure so all three of its
// outcomes are table-testable from any host". Until this split the
// arithmetic below could only be exercised on a macOS runner, and its
// one fixture was hand-authored — it did not carry the page classes the
// arithmetic turned out to depend on (waired-ai/waired-agent#835).

// parseVMStatAvailableBytes approximates available memory from `vm_stat`
// output, in bytes: the reclaimable page classes × the page size
// declared in the header line "(page size of N bytes)". Returns an error
// when the page size or the "Pages free" line cannot be parsed.
//
// The classes are:
//
//   - Pages free / inactive / speculative / purgeable — what macOS will
//     hand out without asking anyone, plus the anonymous pages it will
//     compress first.
//   - the file-backed pages that are NOT already counted above. macOS
//     drops clean file cache on demand, so counting it as unavailable
//     charges the operating system for memory it would give back — the
//     one thing every consumer of this figure is told does not happen
//     ("All three OS probes count reclaimable cache as available",
//     proto/hostfit/hostfit.go Host.OSMemoryDeductionGB, restating
//     docs/decisions/20260808/1907-price-capacity-at-the-served-window.md:
//     「3 OS どれもプリロードキャッシュを空き側に数える」). Linux
//     (MemAvailable) and Windows (AvailPhys) get this from the OS; this
//     sum is the only hand-rolled one, and it was the only one missing
//     the term.
//
// vm_stat reports "File-backed pages" and "Anonymous pages" as a
// partition of active+inactive+speculative — verified exactly on two
// hosts (sv-macmini M4/16 GiB and pc-mbp14-m5 M5 Pro/48 GiB, 2026-08-19)
// — but does NOT say how the file-backed ones split across the active
// and inactive lists. So the term added here is the LOWER bound on the
// file cache that is not already in the sum, max(0, file-backed −
// inactive − speculative); whatever cannot be told apart stays charged
// to the operating system. Consequences, both wanted:
//
//   - on a host whose output has no "File-backed pages" line (older
//     macOS) the term is 0 and the figure is byte-for-byte today's;
//   - the term only becomes positive once the cache is genuinely large,
//     which is exactly the state the install-time measurement is taken
//     in — measured on sv-macmini, reading one 47 GB file moved
//     File-backed 245,581 → 471,249 pages while this function's old
//     answer FELL by ~0.9 GiB.
func parseVMStatAvailableBytes(out []byte) (uint64, error) {
	pageSize, err := parseVMStatPageSize(out)
	if err != nil {
		return 0, err
	}
	counts, err := parseVMStatPageCounts(out)
	if err != nil {
		return 0, err
	}
	return availablePages(counts) * pageSize, nil
}

// availablePages is the page count parseVMStatAvailableBytes reports.
// Named so the real-host test can put it beside the pre-#835 sum on a
// live machine without re-deriving either.
func availablePages(counts map[string]uint64) uint64 {
	return reclaimableWithoutFileCachePages(counts) + activeFileBackedPages(counts)
}

// reclaimableWithoutFileCachePages is the sum this probe returned before
// waired-ai/waired-agent#835: the classes macOS hands out without
// reclaiming any file cache first.
func reclaimableWithoutFileCachePages(counts map[string]uint64) uint64 {
	return counts["Pages free"] + counts["Pages inactive"] +
		counts["Pages speculative"] + counts["Pages purgeable"]
}

// parseVMStatPageCounts reads every "<name>: <integer>." line into a map.
// Lines whose value is not a bare integer are skipped, which is what
// keeps the counters vm_stat also prints ("Translation faults", the
// macOS 26.6 "Pages tag-storage …" block) harmless. "Pages free" is the
// one line that must be there: without it the output is not vm_stat's.
func parseVMStatPageCounts(out []byte) (map[string]uint64, error) {
	counts := map[string]uint64{}
	sawFree := false
	for _, line := range strings.Split(string(out), "\n") {
		key, val, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		num := strings.TrimRight(strings.TrimSpace(val), ".")
		n, perr := strconv.ParseUint(num, 10, 64)
		if perr != nil {
			continue
		}
		k := strings.TrimSpace(key)
		counts[k] = n
		if k == "Pages free" {
			sawFree = true
		}
	}
	if !sawFree {
		return nil, errors.New("vm_stat: 'Pages free' line not found")
	}
	return counts, nil
}

// activeFileBackedPages is the lower bound on file-backed pages that
// parseVMStatAvailableBytes has not already counted: the ones the
// inactive and speculative lists cannot account for. Split out so the
// bound is one named thing a test can pin, rather than an expression
// inside a sum.
func activeFileBackedPages(counts map[string]uint64) uint64 {
	counted := counts["Pages inactive"] + counts["Pages speculative"]
	if fileBacked := counts["File-backed pages"]; fileBacked > counted {
		return fileBacked - counted
	}
	return 0
}

// parseVMStatPageSize extracts the page size (bytes) from the vm_stat
// header line, e.g. "Mach Virtual Memory Statistics: (page size of 16384 bytes)".
func parseVMStatPageSize(out []byte) (uint64, error) {
	const marker = "page size of "
	s := string(out)
	i := strings.Index(s, marker)
	if i < 0 {
		return 0, errors.New("vm_stat: page-size marker not found")
	}
	rest := s[i+len(marker):]
	end := strings.IndexByte(rest, ' ')
	if end < 0 {
		return 0, errors.New("vm_stat: malformed page-size header")
	}
	return strconv.ParseUint(rest[:end], 10, 64)
}

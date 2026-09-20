package mempressure

import (
	"fmt"
	"strconv"
	"strings"
)

// parsePressure reads the avg10 columns out of a Linux pressure file.
//
// The format is two lines, and a cgroup's memory.pressure is byte-for-byte
// the same shape as the host-wide /proc/pressure/memory:
//
//	some avg10=0.00 avg60=0.00 avg300=0.00 total=9063226
//	full avg10=8.51 avg60=1.42 avg300=0.28 total=9030535
//
// Untagged so every platform compiles and tests it: the file is Linux's,
// but deciding what its text means is not a platform decision.
//
// A file with only a "some" line is not an error — cgroup v2 omits "full"
// at the root — and leaves full at zero.
func parsePressure(b []byte) (some, full float64, err error) {
	seen := false
	for _, line := range strings.Split(string(b), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		var target *float64
		switch fields[0] {
		case "some":
			target = &some
		case "full":
			target = &full
		default:
			continue
		}
		raw, ok := strings.CutPrefix(fields[1], "avg10=")
		if !ok {
			return 0, 0, fmt.Errorf("mempressure: %q line does not start with avg10", fields[0])
		}
		v, perr := strconv.ParseFloat(raw, 64)
		if perr != nil {
			return 0, 0, fmt.Errorf("mempressure: %q avg10 %q: %w", fields[0], raw, perr)
		}
		*target = v
		seen = true
	}
	if !seen {
		return 0, 0, fmt.Errorf("mempressure: no some/full line in pressure file")
	}
	return some, full, nil
}

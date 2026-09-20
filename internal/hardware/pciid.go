package hardware

import (
	"regexp"
	"strings"
)

// The PCI vendor:device pair, as the one identity of an accelerator that
// is the same on every operating system (waired-agent#1455).
//
// WHY THIS AND NOT A NAME. The model strings this repo holds are
// whatever the detecting tool printed — `rocm-smi --showproductname` on
// Linux, the registry's DriverDesc on Windows, system_profiler on macOS
// — and they disagree about the same part. GPU.Model's own doc says
// "free-form; do not parse", and waired-agent#251 chose CPU.Model over
// it for that reason. The PCI pair sidesteps the whole question: it is
// not a name, the vendor assigns it, and both operating systems that
// have PCI publish it without privileges:
//
//	Linux   /sys/class/drm/cardN/device/{vendor,device}  -> 0x1002, 0x1586
//	Windows MatchingDeviceId                             -> PCI\VEN_1002&DEV_1586&...
//
// WHAT IT IS FOR. Not the host key — that stays readable, because a
// human reads it in a store. This is the record's structured companion,
// the field an importer can hold against the one already on file to
// decide "same chip?" without trusting a label. It is what closes the
// one gap the readable key has: NVIDIA's chip component is a compute
// capability, and an RTX PRO 4000 Blackwell and an RTX 5090 share 12.0
// while their memory bandwidth differs by nearly 3x. Their PCI pairs do
// not collide.
//
// WHAT IT IS NOT. Not a device identifier. A PCI pair names a PART, and
// every unit of that part reports the same one — which is why it is safe
// in a public repository where a serial or a UUID would not be.

// pciVendorDeviceRe pulls the pair out of a Windows MatchingDeviceId.
// Anchored on the two keywords rather than on position because the
// string carries a variable tail (&SUBSYS_…&REV_…) and, on some
// adapters, no tail at all.
var pciVendorDeviceRe = regexp.MustCompile(`(?i)VEN_([0-9A-F]{4})&DEV_([0-9A-F]{4})`)

// PCIIDFromWindowsMatchingID renders the pair from a Windows display
// adapter's MatchingDeviceId, or "" when the string does not carry one
// (a Remote Display Adapter's is "RdpIdd_IndirectDisplay").
func PCIIDFromWindowsMatchingID(s string) string {
	m := pciVendorDeviceRe.FindStringSubmatch(s)
	if m == nil {
		return ""
	}
	return strings.ToLower(m[1]) + ":" + strings.ToLower(m[2])
}

// PCIIDFromSysfs renders the pair from the two sysfs reads, each of
// which is a "0x" prefixed four-digit hex word with a trailing newline.
// Returns "" when either is missing or malformed.
func PCIIDFromSysfs(vendor, device string) string {
	v, ok := trimSysfsHex(vendor)
	if !ok {
		return ""
	}
	d, ok := trimSysfsHex(device)
	if !ok {
		return ""
	}
	return v + ":" + d
}

// sysfsHexRe is a full match so a truncated read cannot produce a pair
// that looks plausible.
var sysfsHexRe = regexp.MustCompile(`^0x([0-9a-f]{4})$`)

func trimSysfsHex(s string) (string, bool) {
	m := sysfsHexRe.FindStringSubmatch(strings.ToLower(strings.TrimSpace(s)))
	if m == nil {
		return "", false
	}
	return m[1], true
}

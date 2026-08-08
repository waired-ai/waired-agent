package hostfit_test

import (
	"testing"

	"github.com/waired-ai/waired-agent/proto/hostfit"
)

// TestOSMemoryDeductionGB pins the product contract from the 2026-08-08
// owner rulings on waired-agent#568 (field table + rulings comment on
// the issue): the OS deduction is
// max(OSMemoryAllowanceGB, RAMTotalGB − RAMAvailableGB), the
// measurement can only tighten (the floor always holds), and a missing
// or implausible measurement keeps the constant — which is what every
// host behind a pre-#568 agent gets.
func TestOSMemoryDeductionGB(t *testing.T) {
	for _, tc := range []struct {
		name  string
		total int
		avail int
		want  int
	}{
		{"no measurement keeps the constant", 16, 0, hostfit.OSMemoryAllowanceGB},
		{"negative reads as no measurement", 16, -3, hostfit.OSMemoryAllowanceGB},
		{"measurement above total reads as no measurement", 16, 20, hostfit.OSMemoryAllowanceGB},
		{"measurement raises the deduction", 16, 6, 10},
		{"a busy install-time host is charged what it measured", 32, 16, 16},
		{"measurement under the floor keeps the floor", 16, 15, hostfit.OSMemoryAllowanceGB},
		{"avail equal to total keeps the floor", 16, 16, hostfit.OSMemoryAllowanceGB},
		{"failed RAM probe entirely (total 0) keeps the constant", 0, 4, hostfit.OSMemoryAllowanceGB},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := hostfit.Host{RAMTotalGB: tc.total, RAMAvailableGB: tc.avail}
			if got := h.OSMemoryDeductionGB(); got != tc.want {
				t.Errorf("OSMemoryDeductionGB() = %d, want %d", got, tc.want)
			}
		})
	}
}

// TestTotalMemoryMB_MeasuredDeduction pins how the measurement reaches
// the capacity denominator (waired-agent#568). The unmeasured rows of
// TestTotalMemoryMB are untouched on purpose: a host with no
// measurement computes exactly what it computed before #568.
func TestTotalMemoryMB_MeasuredDeduction(t *testing.T) {
	const gb = 1024
	for _, tc := range []struct {
		name string
		host hostfit.Host
		want int
	}{
		{
			"cpu-only: RAM less the measured deduction",
			hostfit.Host{RAMTotalGB: 16, RAMAvailableGB: 6},
			(16 - 10) * gb,
		},
		{
			"cpu-only: a clean host keeps the floor",
			hostfit.Host{RAMTotalGB: 16, RAMAvailableGB: 15},
			(16 - hostfit.OSMemoryAllowanceGB) * gb,
		},
		{
			"a machine measured smaller than the floor has nothing",
			hostfit.Host{RAMTotalGB: 4, RAMAvailableGB: 1},
			(4 - 3) * gb,
		},
		{
			"discrete: only the RAM term shrinks, the card is untouched",
			hostfit.Host{RAMTotalGB: 32, RAMAvailableGB: 16, GPUCount: 1, VRAM0MB: 8192},
			(32-16)*gb + 8192,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.host.TotalMemoryMB(); got != tc.want {
				t.Errorf("TotalMemoryMB() = %d, want %d", got, tc.want)
			}
		})
	}
}

// TestFromHardwareSummary_RAMAvailable proves the control plane's
// adapter reads the wire field, from the wire form (fixtures are wire
// strings, waired#950): the CP and the agent compute the same verdict
// from the same published integer.
func TestFromHardwareSummary_RAMAvailable(t *testing.T) {
	h := hostFromWire(t, `{"ram_total_gb":16,"ram_available_gb":6}`)
	if h.RAMAvailableGB != 6 {
		t.Fatalf("RAMAvailableGB = %d, want 6", h.RAMAvailableGB)
	}
	if got, want := h.TotalMemoryMB(), (16-10)*1024; got != want {
		t.Errorf("TotalMemoryMB() = %d, want %d", got, want)
	}
	// Pre-#568 wire: the field is absent, the constant governs.
	old := hostFromWire(t, `{"ram_total_gb":16}`)
	if got, want := old.TotalMemoryMB(), (16-hostfit.OSMemoryAllowanceGB)*1024; got != want {
		t.Errorf("pre-addition TotalMemoryMB() = %d, want %d", got, want)
	}
}

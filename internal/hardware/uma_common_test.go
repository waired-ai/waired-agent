package hardware

import "testing"

func TestIsStrixHaloAPU(t *testing.T) {
	cases := []struct {
		name  string
		model string
		want  bool
	}{
		{"strix halo 395", "AMD Ryzen AI Max 395 w/ Radeon 8060S", true},
		{"strix halo 395+ pro", "AMD Ryzen AI Max+ PRO 395", true},
		{"strix halo windows registry capitalisation", "AMD RYZEN AI MAX+ 395 w/ Radeon 8060S", true},
		{"case insensitive", "amd ryzen ai max 395", true},
		{"Phoenix not Strix Halo", "AMD Ryzen 9 7940HS w/ Radeon 780M Graphics", false},
		{"Intel ignored", "13th Gen Intel(R) Core(TM) i7-13700K", false},
		{"empty", "", false},
		{"AI but not Max", "AMD Ryzen AI 9 365", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := IsStrixHaloAPU(c.model); got != c.want {
				t.Errorf("IsStrixHaloAPU(%q) = %v, want %v", c.model, got, c.want)
			}
		})
	}
}

func TestMinNonZero(t *testing.T) {
	cases := []struct {
		in   []int
		want int
	}{
		{[]int{10, 20, 30}, 10},
		{[]int{0, 20, 30}, 20},
		{[]int{0, 0, 0}, 0},
		{[]int{-5, 0, 100}, 100},
		{[]int{50}, 50},
		{[]int{}, 0},
	}
	for _, c := range cases {
		if got := minNonZero(c.in...); got != c.want {
			t.Errorf("minNonZero(%v) = %d, want %d", c.in, got, c.want)
		}
	}
}

// TestStrixHaloUsableVRAMMB pins the carve-out-vs-heuristic logic shared
// by the Linux and Windows UMA detectors. The key invariant: when a
// carve-out reading (amdVRAMMB) is present it is authoritative (clamped
// to the BIOS ceiling) and the 75 %-of-RAM heuristic is NOT consulted —
// that is the bug fix for BIOS carve-out machines whose OS-visible RAM
// is only the leftover after the GPU allocation.
func TestStrixHaloUsableVRAMMB(t *testing.T) {
	const capMB = 96 * 1024
	cases := []struct {
		name       string
		amdVRAMMB  int
		ramTotalGB int
		want       int
	}{
		{
			// The real Ryzen AI Max+ 395 carve-out: 96 GB to the iGPU,
			// only ~31 GB left to the OS. Old code: min(96, 23, 96)=23.
			name:      "carve-out present, leftover RAM small → carve-out wins",
			amdVRAMMB: 96 * 1024, ramTotalGB: 31, want: capMB,
		},
		{
			name:      "carve-out present below cap → carve-out value",
			amdVRAMMB: 64 * 1024, ramTotalGB: 128, want: 64 * 1024,
		},
		{
			name:      "carve-out present above cap → clamped to cap",
			amdVRAMMB: 200 * 1024, ramTotalGB: 256, want: capMB,
		},
		{
			name:      "no carve-out, truly-unified host → 75% heuristic",
			amdVRAMMB: 0, ramTotalGB: 32, want: 24 * 1024,
		},
		{
			name:      "no carve-out, large RAM → heuristic clamped to cap",
			amdVRAMMB: 0, ramTotalGB: 256, want: capMB,
		},
		{
			// Everything failed (no GPU reading, no RAM): preserve the
			// prior behaviour of returning the ceiling as a last resort
			// rather than 0 (which would read as "CPU only").
			name:      "no carve-out, no RAM → cap fallback",
			amdVRAMMB: 0, ramTotalGB: 0, want: capMB,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := strixHaloUsableVRAMMB(c.amdVRAMMB, c.ramTotalGB)
			if got != c.want {
				t.Errorf("strixHaloUsableVRAMMB(%d, %d) = %d, want %d",
					c.amdVRAMMB, c.ramTotalGB, got, c.want)
			}
		})
	}
}

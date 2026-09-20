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

func TestIsAMDMobileAPU(t *testing.T) {
	cases := []struct {
		name  string
		model string
		want  bool
	}{
		{"phoenix 780M", "AMD Ryzen 9 7940HS w/ Radeon 780M Graphics", true},
		{"hawk point 780M", "AMD Ryzen 7 8840U w/ Radeon 780M Graphics", true},
		{"phoenix 760M", "AMD Ryzen 5 7640U w/ Radeon 760M Graphics", true},
		{"desktop APU 780M", "AMD Ryzen 7 8700G w/ Radeon 780M Graphics", true},
		{"strix point 890M", "AMD Ryzen AI 9 HX 370 w/ Radeon 890M", true},
		// Strix Halo: no three-digit "…M" token; also caught upstream by
		// IsStrixHaloAPU before this is consulted.
		{"strix halo 8060S not mobile-APU here", "AMD Ryzen AI Max+ 395 w/ Radeon 8060S", false},
		// Vestigial desktop iGPU: bare "Radeon Graphics", no number —
		// engaging a ~2 CU iGPU can be slower than the CPU.
		{"desktop vestigial radeon graphics", "AMD Ryzen 9 7950X 16-Core Processor w/ Radeon Graphics", false},
		{"desktop no igpu", "AMD Ryzen 9 5950X 16-Core Processor", false},
		{"epyc server", "AMD EPYC 7763 64-Core Processor", false},
		{"intel ignored", "13th Gen Intel(R) Core(TM) i7-13700K", false},
		{"empty", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := IsAMDMobileAPU(c.model); got != c.want {
				t.Errorf("IsAMDMobileAPU(%q) = %v, want %v", c.model, got, c.want)
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

// TestStrixHaloUMA pins the per-OS carve-out logic shared by the Linux
// and Windows UMA detectors. It is a record of today's behaviour on each
// OS, not a platform contract: the Windows rows come from one measured
// host (waired-ai/waired-agent#863) and the Linux rows were never
// measured at all (waired-ai/waired-agent#864).
//
// Linux: a carve-out reading (amdVRAMMB) is authoritative, clamped to
// the BIOS ceiling, and the 75 %-of-RAM heuristic is NOT consulted. The
// second return is the same value there and 0 on the heuristic branch,
// and that zero is what stops hostfit.TotalMemoryMB adding a slice of
// RAM to the RAM it was sliced from.
//
// Windows: the carve-out reading is ignored in both positions. Every
// graphics allocation carries a system-memory backing store of equal
// size, so the budget is the OS-visible RAM minus the OS deduction and
// the carve-out is never additive.
func TestStrixHaloUMA(t *testing.T) {
	const capMB = 96 * 1024
	cases := []struct {
		name         string
		goos         string
		amdVRAMMB    int
		ramTotalGB   int
		ramAvailGB   int
		want         int
		wantCarveOut int
	}{
		{
			// The real Ryzen AI Max+ 395 carve-out: 96 GB to the iGPU,
			// only ~31 GB left to the OS.
			name: "linux: carve-out present, leftover RAM small -> carve-out wins",
			goos: "linux", amdVRAMMB: 96 * 1024, ramTotalGB: 31,
			want: capMB, wantCarveOut: capMB,
		},
		{
			name: "linux: carve-out present below cap -> carve-out value",
			goos: "linux", amdVRAMMB: 64 * 1024, ramTotalGB: 128,
			want: 64 * 1024, wantCarveOut: 64 * 1024,
		},
		{
			name: "linux: carve-out present above cap -> clamped to cap",
			goos: "linux", amdVRAMMB: 200 * 1024, ramTotalGB: 256,
			want: capMB, wantCarveOut: capMB,
		},
		{
			name: "linux: no carve-out, truly-unified host -> 75% heuristic, nothing to add",
			goos: "linux", amdVRAMMB: 0, ramTotalGB: 32,
			want: 24 * 1024, wantCarveOut: 0,
		},
		{
			name: "linux: no carve-out, large RAM -> heuristic clamped to cap, nothing to add",
			goos: "linux", amdVRAMMB: 0, ramTotalGB: 256,
			want: capMB, wantCarveOut: 0,
		},
		{
			// Inverted vs. the pre-#863 table, which returned the ceiling
			// here: minNonZero treats 0 as "not a candidate", so a host
			// that measured nothing used to publish the largest budget
			// this code can express.
			name: "linux: nothing measured -> unknown, not the ceiling",
			goos: "linux", amdVRAMMB: 0, ramTotalGB: 0,
			want: 0, wantCarveOut: 0,
		},
		{
			// darwin never calls this — profiler_darwin.go has its own
			// defaultUMA and leaves CarveOutVRAMMB 0 (waired-ai/waired#1056
			// decision 1). The row is here so the table covers all three
			// GOOS values and records that only Windows diverges.
			name: "darwin: same answer as linux, and it is unreachable in production",
			goos: "darwin", amdVRAMMB: 96 * 1024, ramTotalGB: 31,
			want: capMB, wantCarveOut: capMB,
		},
		{
			// The measured failing configuration: 96 GB to the iGPU, the
			// OS left with ~31 GB, and a 76.3 GB model that could not
			// load. 29 GiB is what the load path actually had.
			name: "windows: 96 GB carve-out is not the budget, the leftover RAM is",
			goos: "windows", amdVRAMMB: 96 * 1024, ramTotalGB: 31,
			want: 29 * 1024, wantCarveOut: 0,
		},
		{
			// The measured working configuration: carve-out shrunk to
			// 512 MB, so the OS sees the whole 128 GB machine.
			name: "windows: tiny carve-out does not shrink the budget",
			goos: "windows", amdVRAMMB: 512, ramTotalGB: 127,
			want: capMB, wantCarveOut: 0,
		},
		{
			name: "windows: carve-out reading is ignored in both positions",
			goos: "windows", amdVRAMMB: 64 * 1024, ramTotalGB: 32,
			want: 30 * 1024, wantCarveOut: 0,
		},
		{
			name: "windows: registry unreadable changes nothing",
			goos: "windows", amdVRAMMB: 0, ramTotalGB: 32,
			want: 30 * 1024, wantCarveOut: 0,
		},
		{
			// A measured install-time available figure raises the OS
			// deduction above the 2 GB floor, exactly as
			// hostfit.Host.OSMemoryDeductionGB does for the capacity gate.
			name: "windows: a measured OS deduction is used, not the floor",
			goos: "windows", amdVRAMMB: 96 * 1024, ramTotalGB: 31, ramAvailGB: 24,
			want: 24 * 1024, wantCarveOut: 0,
		},
		{
			name: "windows: RAM probe failed -> unknown",
			goos: "windows", amdVRAMMB: 96 * 1024, ramTotalGB: 0,
			want: 0, wantCarveOut: 0,
		},
		{
			name: "windows: nothing left after the OS deduction -> unknown",
			goos: "windows", amdVRAMMB: 96 * 1024, ramTotalGB: 2,
			want: 0, wantCarveOut: 0,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, carveOut := strixHaloUMA(c.goos, c.amdVRAMMB, c.ramTotalGB, c.ramAvailGB)
			if got != c.want || carveOut != c.wantCarveOut {
				t.Errorf("strixHaloUMA(%q, %d, %d, %d) = (%d, %d), want (%d, %d)",
					c.goos, c.amdVRAMMB, c.ramTotalGB, c.ramAvailGB,
					got, carveOut, c.want, c.wantCarveOut)
			}
		})
	}
}

// TestStrixHaloUMA_WindowsDivergesFromLinux keeps the goos parameter from
// becoming decorative. If the Windows branch is ever deleted or its
// condition narrowed until nothing reaches it, the table above would
// still pass on its Linux rows while every Windows row silently changed;
// this asserts the two answers differ on the input that motivated the
// split, in both returned figures.
func TestStrixHaloUMA_WindowsDivergesFromLinux(t *testing.T) {
	const carveOutMB = 96 * 1024
	const ramGB = 31

	linuxUsable, linuxCarveOut := strixHaloUMA("linux", carveOutMB, ramGB, 0)
	winUsable, winCarveOut := strixHaloUMA("windows", carveOutMB, ramGB, 0)

	if linuxUsable == winUsable {
		t.Errorf("both OSes returned usable=%d; the Windows branch is not reached", winUsable)
	}
	if linuxCarveOut == winCarveOut {
		t.Errorf("both OSes returned carveOut=%d; the Windows branch is not reached", winCarveOut)
	}
	if winCarveOut != 0 {
		t.Errorf("windows carveOut = %d, want 0: hostfit.TotalMemoryMB would add it to RAM", winCarveOut)
	}
	if winUsable >= carveOutMB {
		t.Errorf("windows usable = %d, want less than the %d MB carve-out it must not trust",
			winUsable, carveOutMB)
	}
}

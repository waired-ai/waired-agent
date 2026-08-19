//go:build darwin

package hardware

import (
	"context"
	"os"
	"testing"
	"time"
)

// TestRealHostVMStatAvailability runs the availability probe against THIS
// Mac's live vm_stat and reports the answer beside the pre-#835 sum.
//
// It exists because the change in waired-ai/waired-agent#835 is a claim
// about real machines — "macOS reclaims the file cache, so counting it as
// unavailable under-reports memory" — and captured fixtures can only show
// that the arithmetic does what it says. One binary reports both figures,
// so a before/after does not need two builds.
//
// Gated by WAIRED_HW_REALHOST=1 like TestRealHostProfileAppleSilicon, and
// it asserts only the invariants that hold on ANY host: the new figure is
// never below the old one, and never above total RAM. The interesting
// numbers are logged, not asserted, because they are properties of
// whatever the machine happens to be doing.
//
// To take a measurement:
//
//	WAIRED_HW_REALHOST=1 go test ./internal/hardware/ -run RealHostVMStat -v
//	# then warm the file cache and run it again:
//	dd if=<a multi-GB file> of=/dev/null bs=1m count=20000
//	WAIRED_HW_REALHOST=1 go test ./internal/hardware/ -run RealHostVMStat -v
func TestRealHostVMStatAvailability(t *testing.T) {
	if os.Getenv("WAIRED_HW_REALHOST") == "" {
		t.Skip("set WAIRED_HW_REALHOST=1 to exercise the real hardware probe")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	out, err := vmStat(ctx)
	if err != nil {
		t.Fatalf("vm_stat: %v", err)
	}
	t.Logf("raw vm_stat output:\n%s", out)

	pageSize, err := parseVMStatPageSize(out)
	if err != nil {
		t.Fatalf("page size: %v", err)
	}
	counts, err := parseVMStatPageCounts(out)
	if err != nil {
		t.Fatalf("page counts: %v", err)
	}

	const gib = float64(uint64(1) << 30)
	toGiB := func(pages uint64) float64 { return float64(pages*pageSize) / gib }

	before := reclaimableWithoutFileCachePages(counts)
	cache := activeFileBackedPages(counts)
	after := availablePages(counts)

	t.Logf("page size %d bytes", pageSize)
	t.Logf("available: pre-#835 %.2f GiB (%d pages), now %.2f GiB (%d pages), "+
		"of which provable file cache %.2f GiB (%d pages)",
		toGiB(before), before, toGiB(after), after, toGiB(cache), cache)

	// The identity activeFileBackedPages is a bound under. Reported, not
	// asserted: a future macOS is allowed to change what vm_stat prints,
	// and this test's job is to say so, not to fail a release.
	resident := counts["Pages active"] + counts["Pages inactive"] + counts["Pages speculative"]
	if partition := counts["File-backed pages"] + counts["Anonymous pages"]; partition != resident {
		t.Logf("NOTE: File-backed + Anonymous = %d but active+inactive+speculative = %d. "+
			"activeFileBackedPages assumes these are the same set; if this host is not an "+
			"outlier, that bound needs revisiting (waired-ai/waired-agent#835).",
			partition, resident)
	}

	if after < before {
		t.Errorf("available fell from %d to %d pages; the file-cache term must only add", before, after)
	}

	total, avail, err := defaultRAM(ctx)
	if err != nil {
		t.Fatalf("defaultRAM: %v", err)
	}
	t.Logf("defaultRAM: RAMTotalGB=%d RAMAvailableGB=%d", total, avail)
	if avail > total {
		t.Errorf("RAMAvailableGB = %d > RAMTotalGB = %d", avail, total)
	}
}

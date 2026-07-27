package runtime

import (
	"testing"
	"time"
)

// The fixture lines below are VERBATIM uv 0.11.26 output, captured by
// running the installer's exact `uv pip install vllm==0.24.0 ...`
// command line with both streams piped (the shape DefaultInstallRunner
// gives it). Keeping them real is the point: the percent fixtures this
// package shipped before waired-agent#255 were invented, and they hid
// the fact that uv emits no percentage at all through a pipe.
//
// Contract, not a record of today's behaviour: if uv changes this
// format, these tests are supposed to fail — the wizard's byte bar is
// drawn from nothing else.

func TestUVDownloadTrackerReadsRealUVOutput(t *testing.T) {
	// A real interleaved slice: uv announces as each transfer starts and
	// confirms as each finishes, so announcements and completions arrive
	// mixed rather than in phases.
	lines := []string{
		"Resolved 190 packages in 2.47s",
		"Downloading torch (506.1MiB)",
		"Downloading nvidia-nvjitlink (38.8MiB)",
		" Downloaded nvidia-nvjitlink",
		"Downloading apache-tvm-ffi (2.2MiB)",
		" Downloaded torch",
		" Downloaded apache-tvm-ffi",
	}
	wantTotal := mib(506.1) + mib(38.8) + mib(2.2)

	tr := newUVDownloadTracker()
	base := time.Unix(1700000000, 0)
	for i, l := range lines {
		tr.Observe(l, base.Add(time.Duration(i)*time.Second))
	}

	completed, total, _ := tr.Snapshot()
	if total != wantTotal {
		t.Errorf("total = %d, want %d", total, wantTotal)
	}
	// Every announced package finished, so the row is complete.
	if completed != wantTotal {
		t.Errorf("completed = %d, want %d (all three finished)", completed, wantTotal)
	}
}

func TestUVDownloadTrackerCountsOnlyAnnouncedPackages(t *testing.T) {
	// uv announces only the packages above its own size threshold — 63 of
	// the 190 vLLM resolves. A completion for a package that was never
	// announced carries no bytes to attribute, so it must move neither
	// side of the ratio.
	tr := newUVDownloadTracker()
	now := time.Unix(1700000000, 0)

	tr.Observe("Downloading torch (506.1MiB)", now)
	tr.Observe(" Downloaded charset-normalizer", now) // never announced
	completed, total, _ := tr.Snapshot()

	if completed != 0 {
		t.Errorf("completed = %d, want 0 — an unannounced completion is not progress", completed)
	}
	if total != mib(506.1) {
		t.Errorf("total = %d, want torch's size alone", total)
	}
}

func TestUVDownloadTrackerIgnoresRepeats(t *testing.T) {
	tr := newUVDownloadTracker()
	now := time.Unix(1700000000, 0)

	tr.Observe("Downloading torch (506.1MiB)", now)
	tr.Observe("Downloading torch (506.1MiB)", now)
	tr.Observe(" Downloaded torch", now)
	tr.Observe(" Downloaded torch", now)

	completed, total, _ := tr.Snapshot()
	if want := mib(506.1); completed != want || total != want {
		t.Errorf("completed/total = %d/%d, want %d/%d — a repeated line must not double-count",
			completed, total, want, want)
	}
}

func TestUVDownloadTrackerPreparedClosesTheGap(t *testing.T) {
	// "Prepared N packages" is uv's own statement that every wheel is in.
	// It closes the row so a bar cannot park below 100% because one
	// completion line was missed.
	tr := newUVDownloadTracker()
	now := time.Unix(1700000000, 0)

	tr.Observe("Downloading torch (506.1MiB)", now)
	tr.Observe("Downloading nvidia-nvjitlink (38.8MiB)", now)
	tr.Observe(" Downloaded torch", now.Add(time.Second))
	if changed := tr.Observe("Prepared 190 packages in 1m 20s", now.Add(2*time.Second)); !changed {
		t.Fatal("Prepared did not move the figures")
	}

	completed, total, _ := tr.Snapshot()
	if completed != total {
		t.Errorf("completed = %d, total = %d — Prepared must finish the row", completed, total)
	}
}

func TestUVDownloadTrackerNoDownloadsStaysUnknown(t *testing.T) {
	// A host whose uv cache already holds every wheel announces nothing.
	// total 0 is the wire's "size unknown", which the wizard renders
	// indeterminate rather than as a bar of made-up length.
	tr := newUVDownloadTracker()
	now := time.Unix(1700000000, 0)

	for _, l := range []string{
		"Resolved 190 packages in 2.47s",
		"Prepared 190 packages in 1m 20s",
		"Installed 190 packages in 53ms",
		" + vllm==0.24.0",
	} {
		tr.Observe(l, now)
	}

	completed, total, bps := tr.Snapshot()
	if completed != 0 || total != 0 || bps != 0 {
		t.Errorf("completed/total/bps = %d/%d/%d, want 0/0/0", completed, total, bps)
	}
}

func TestUVDownloadTrackerRate(t *testing.T) {
	tr := newUVDownloadTracker()
	base := time.Unix(1700000000, 0)

	// 100 MiB announced and finished over 10 s inside the window.
	tr.Observe("Downloading a (100.0MiB)", base)
	tr.Observe(" Downloaded a", base.Add(10*time.Second))

	_, _, bps := tr.Snapshot()
	want := mib(100) / 10
	// Integer truncation in the rate maths, not a tolerance on the
	// measurement: the inputs here are exact.
	if bps < want-1 || bps > want+1 {
		t.Errorf("bps = %d, want ~%d", bps, want)
	}
}

func TestUVDownloadTrackerRateUnknownUntilTwoSamples(t *testing.T) {
	// 0 is the wire's value for "unknown", and it is also what a stalled
	// transfer reports — waired-agent#197 settled that a stall is read off
	// completed not advancing, so nothing needs to distinguish them here.
	tr := newUVDownloadTracker()
	tr.Observe("Downloading a (100.0MiB)", time.Unix(1700000000, 0))

	if _, _, bps := tr.Snapshot(); bps != 0 {
		t.Errorf("bps = %d with one sample, want 0", bps)
	}
}

func TestParseUVDownloading(t *testing.T) {
	cases := []struct {
		line     string
		wantName string
		wantSize int64
		wantOK   bool
	}{
		{"Downloading torch (506.1MiB)", "torch", mib(506.1), true},
		{"Downloading nvidia-cudnn-cu13 (349.1MiB)", "nvidia-cudnn-cu13", mib(349.1), true},
		{"Downloading tiny (4.2KiB)", "tiny", kib(4.2), true},
		{"Downloading huge (2.5GiB)", "huge", gib(2.5), true},
		{"Downloading raw (512B)", "raw", 512, true},
		// Malformed / not an announcement.
		{"Downloading torch", "", 0, false},
		{"Downloading (506.1MiB)", "", 0, false},
		{"Downloading torch (nonsense)", "", 0, false},
		{"Downloading torch (506.1)", "", 0, false},
	}
	for _, c := range cases {
		name, size, ok := parseUVDownloading(c.line)
		if ok != c.wantOK || name != c.wantName || size != c.wantSize {
			t.Errorf("parseUVDownloading(%q) = (%q, %d, %v), want (%q, %d, %v)",
				c.line, name, size, ok, c.wantName, c.wantSize, c.wantOK)
		}
	}
}

// mib / kib / gib mirror download.ParseSize's arithmetic (float multiply,
// then truncate) so the expectations are exactly what the parser
// produces rather than a rounding of it.
func mib(f float64) int64 { return int64(f * (1 << 20)) }
func kib(f float64) int64 { return int64(f * (1 << 10)) }
func gib(f float64) int64 { return int64(f * (1 << 30)) }

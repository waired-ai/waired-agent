package servicediag

import (
	"strings"
	"testing"
	"time"
)

// rc8CodeIntegrity3077 is the EventData array a real Windows 11 host
// produced for the waired-agent block, in the order wevtutil emitted it
// (#653). Hashes and the policy GUID are replaced with same-shaped
// placeholders — this repo is public and CI runs a secret scan — but the
// LAYOUT is untouched, which is the only part the code reads.
//
// Two device paths: the blocked image first, the process that asked for it
// second. Everything else is length prefixes, hashes, a signing level, a
// policy GUID and trailing zeroes.
var rc8CodeIntegrity3077 = []EventField{
	{Value: "61"},
	{Value: `\Device\HarddiskVolume3\Program Files\Waired\waired-agent.exe`},
	{Value: "59"},
	{Value: `\Device\HarddiskVolume3\Program Files\PowerShell\7\pwsh.exe`},
	{Value: "2"},
	{Value: "1"},
	{Value: "0xc0e90002"},
	{Value: "20"},
	{Value: strings.Repeat("a", 40)},
	{Value: "32"},
	{Value: strings.Repeat("b", 64)},
	{Value: "20"},
	{Value: strings.Repeat("c", 40)},
	{Value: "32"},
	{Value: strings.Repeat("d", 64)},
	{Value: "VerifiedAndReputableDesktop"},
	{Value: "17"},
	{Value: "27555.1000.240208"},
	{Value: "32"},
	{Value: strings.Repeat("e", 64)},
	{Value: "0"},
	{Value: "0"},
	{Value: "0"},
	{Value: "0"},
	{Value: "0.0.0.0"},
	{Value: "{00000000-0000-0000-0000-000000000000}"},
	{Value: "false"},
	{Value: "0"},
}

// The named form, which is what wevtutil emits when the provider manifest
// is registered. Same record, same conclusion — the summary must not
// depend on which shape the host produces.
var rc8CodeIntegrity3077Named = []EventField{
	{Name: "FileNameLength", Value: "61"},
	{Name: "FileNameBuffer", Value: `\Device\HarddiskVolume3\Program Files\Waired\waired-agent.exe`},
	{Name: "ProcessNameLength", Value: "59"},
	{Name: "ProcessNameBuffer", Value: `\Device\HarddiskVolume3\Program Files\PowerShell\7\pwsh.exe`},
	{Name: "Requested Signing Level", Value: "2"},
	{Name: "Validated Signing Level", Value: "1"},
	{Name: "Status", Value: "0xc0e90002"},
	{Name: "SHA1 Hash Size", Value: "20"},
	{Name: "SHA1 Hash", Value: strings.Repeat("a", 40)},
	{Name: "PolicyGUID", Value: "{00000000-0000-0000-0000-000000000000}"},
}

// TestSummarizeCodeIntegrity pins #653: the bracketed evidence tail used
// to be the whole positional array joined with spaces — length prefixes,
// two device paths, four hashes, a signing level, a policy GUID and a run
// of zeroes, on one line. The prose before it was fine; the tail swamped
// the terminal.
//
// Product contract from #653, not a record of today's behaviour.
func TestSummarizeCodeIntegrity(t *testing.T) {
	for _, c := range []struct {
		name   string
		fields []EventField
	}{
		{"positional (no provider manifest)", rc8CodeIntegrity3077},
		{"named (manifest registered)", rc8CodeIntegrity3077Named},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := summarizeCodeIntegrity(c.fields)

			// What the operator can act on survives.
			if !strings.Contains(got, `\Program Files\Waired\waired-agent.exe`) {
				t.Errorf("summary = %q, drops the blocked file", got)
			}
			if !strings.Contains(got, "pwsh.exe") {
				t.Errorf("summary = %q, drops the process that requested it", got)
			}
			// The noise does not.
			for _, unwanted := range []string{
				strings.Repeat("a", 40), // SHA1
				strings.Repeat("b", 64), // SHA256
				"{00000000-0000-0000-0000-000000000000}",
				"0xc0e90002",
			} {
				if strings.Contains(got, unwanted) {
					t.Errorf("summary = %q, still carries %q", got, unwanted)
				}
			}
			// A one-line clause, not a dump. The real array joined is
			// ~450 characters.
			if len(got) > 200 {
				t.Errorf("summary is %d chars: %q", len(got), got)
			}
		})
	}
}

// An unrecognised layout must not silently lose the evidence — the caller
// falls back to the raw join when the summary is empty.
func TestSummarizeCodeIntegrity_UnrecognisedLayout(t *testing.T) {
	if got := summarizeCodeIntegrity([]EventField{{Value: "42"}, {Value: "something"}}); got != "" {
		t.Errorf("summary = %q, want empty so the caller can fall back", got)
	}
}

// A record naming only the blocked file still says what it can.
func TestSummarizeCodeIntegrity_FileOnly(t *testing.T) {
	got := summarizeCodeIntegrity([]EventField{
		{Name: "FileNameBuffer", Value: `\Device\HarddiskVolume3\Program Files\Waired\waired-agent.exe`},
	})
	if !strings.Contains(got, "waired-agent.exe") || strings.Contains(got, "requested by") {
		t.Errorf("summary = %q", got)
	}
}

// TestRecentEvents pins the other half of #653: the rc8 host's doctor
// cited a CodeIntegrity block from 2026-06-29 — six weeks and several
// reinstalls of the service earlier — as the reason a service that was
// running fine carried a ⚠. Callers pass the current boot as the cutoff.
func TestRecentEvents(t *testing.T) {
	boot := time.Date(2026, 8, 11, 9, 0, 0, 0, time.UTC)
	stale := Event{Source: "Microsoft-Windows-CodeIntegrity", ID: 3077,
		Message: "blocked x", When: time.Date(2026, 6, 29, 12, 0, 0, 0, time.UTC)}
	fresh := Event{Source: "Service Control Manager", ID: 7000,
		Message: "failed to start", When: time.Date(2026, 8, 11, 9, 0, 30, 0, time.UTC)}
	undated := Event{Source: "launchd", Message: "state = running"}

	got := recentEvents([]Event{stale, fresh, undated}, boot)

	if len(got) != 2 {
		t.Fatalf("kept %d events, want 2: %+v", len(got), got)
	}
	for _, e := range got {
		if e.ID == 3077 {
			t.Errorf("a record from before this boot survived: %+v", e)
		}
	}
	// Undated evidence is current-state, not history: the Unix collectors
	// report properties, and dropping them would silence those platforms.
	if got[1].Source != "launchd" {
		t.Errorf("undated event was dropped: %+v", got)
	}
}

// The whole point is that a stale record stops explaining anything: with
// nothing left after the window, Explain says nothing rather than blaming
// a six-week-old event.
func TestExplain_WindowsStaleEventExplainsNothing(t *testing.T) {
	boot := time.Date(2026, 8, 11, 9, 0, 0, 0, time.UTC)
	events := recentEvents([]Event{{
		Source: "Microsoft-Windows-CodeIntegrity", ID: 3077,
		Message: `blocked \Device\HarddiskVolume3\Program Files\Waired\waired-agent.exe`,
		When:    time.Date(2026, 6, 29, 12, 0, 0, 0, time.UTC),
	}}, boot)

	got := Explain("windows", true, events)
	if got.Status != Healthy {
		t.Errorf("Status=%v, want Healthy", got.Status)
	}
	if got.Evidence != "" {
		t.Errorf("Evidence=%q: a record from before this boot is not evidence about now", got.Evidence)
	}
}

// TestEventMessage covers the collector's choice of what to quote. It
// lives here, untagged, because collect_windows.go never compiles on the
// CI leg that runs these tests — the seam has to sit below the build tag
// or the decision is never exercised (CLAUDE.md §Test discipline).
func TestEventMessage(t *testing.T) {
	raw := "61 x 59 y"
	t.Run("code integrity records are summarised", func(t *testing.T) {
		got := eventMessage(winCodeIntegrityAudit, rc8CodeIntegrity3077, "…the whole array…")
		if !strings.Contains(got, "blocked ") || strings.Contains(got, "the whole array") {
			t.Errorf("message = %q, want the summary", got)
		}
	})
	t.Run("an unsummarisable code integrity record keeps its evidence", func(t *testing.T) {
		if got := eventMessage(winCodeIntegrityBlocked, []EventField{{Value: "42"}}, raw); got != raw {
			t.Errorf("message = %q, want the raw join as a fallback", got)
		}
	})
	t.Run("SCM records already carry prose", func(t *testing.T) {
		prose := "The Waired Agent service failed to start."
		if got := eventMessage(winSCMStartFailed, nil, prose); got != prose {
			t.Errorf("message = %q, want it untouched", got)
		}
	})
}

func TestParseSystemTime(t *testing.T) {
	want := time.Date(2026, 6, 29, 12, 0, 0, 0, time.UTC)
	if got := parseSystemTime("2026-06-29T12:00:00.0000000Z"); !got.Equal(want) {
		t.Errorf("parseSystemTime = %v, want %v", got, want)
	}
	// An unreadable clock must not drop the record: losing the diagnosis
	// is worse than citing an event of unknown age.
	for _, in := range []string{"", "not a time"} {
		if got := parseSystemTime(in); !got.IsZero() {
			t.Errorf("parseSystemTime(%q) = %v, want the zero time", in, got)
		}
	}
}

// The evidence line renders the record's own time, at minute precision.
// The provider hands over RFC3339 with sub-second digits, which was part
// of the unreadable tail.
func TestEventLine_RendersTheRecordTime(t *testing.T) {
	when := time.Date(2026, 8, 11, 9, 15, 30, 123456789, time.UTC)
	line := Event{Source: "Microsoft-Windows-CodeIntegrity", ID: 3077,
		Message: "blocked x", When: when}.line()
	if !strings.Contains(line, when.Local().Format("2006-01-02 15:04")) {
		t.Errorf("line = %q, does not carry the record time", line)
	}
	if strings.Contains(line, "123456789") || strings.Contains(line, "T") {
		t.Errorf("line = %q, still carries the raw RFC3339 stamp", line)
	}
	// An undated event keeps the old shape exactly.
	plain := Event{Source: "systemd", Message: "Result=exit-code"}.line()
	if plain != "systemd: Result=exit-code" {
		t.Errorf("undated line = %q, want the unchanged form", plain)
	}
}

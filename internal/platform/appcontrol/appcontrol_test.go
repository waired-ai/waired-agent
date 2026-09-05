package appcontrol

import (
	"strings"
	"testing"
	"time"
)

// A Smart App Control refusal cannot be summoned: it arrives when Microsoft's
// service decides it does, and it lifts on its own
// (docs/knowledges/20260904/0310-a-permissive-window-makes-every-test-pass.md
// is about exactly that hazard). So the evidence here is captured event data
// from the host in waired-agent#1217, with the device volume number and the
// hashes replaced — this repo is public — and the LAYOUT untouched, which is
// the only part the code reads.

// blockXML is one 3077 in the shape a Windows 11 host emits with the provider
// manifest registered: named fields.
func blockXML(when, file, requester string) string {
	return `<Event xmlns='http://schemas.microsoft.com/win/2004/08/events/event'>` +
		`<System><Provider Name='Microsoft-Windows-CodeIntegrity'/><EventID>3077</EventID>` +
		`<TimeCreated SystemTime='` + when + `'/></System>` +
		`<EventData>` +
		`<Data Name='FileNameBuffer'>` + file + `</Data>` +
		`<Data Name='ProcessNameBuffer'>` + requester + `</Data>` +
		`<Data Name='SignatureState'>0</Data>` +
		`</EventData></Event>`
}

// detailXML is the 3118 that follows a block. cloudCode is
// DefenderCloudHTTPCode: 0x0 means the verdict came from this device's cache.
func detailXML(when, file, cloudCode string) string {
	return `<Event xmlns='http://schemas.microsoft.com/win/2004/08/events/event'>` +
		`<System><Provider Name='Microsoft-Windows-CodeIntegrity'/><EventID>3118</EventID>` +
		`<TimeCreated SystemTime='` + when + `'/></System>` +
		`<EventData>` +
		`<Data Name='FileNameBuffer'>` + file + `</Data>` +
		`<Data Name='DefenderCloudHTTPCode'>` + cloudCode + `</Data>` +
		`<Data Name='DefenderTrust'>-16777216</Data>` +
		`<Data Name='IsUnfriendlyFile'>false</Data>` +
		`</EventData></Event>`
}

const (
	cliPath   = `\Device\HarddiskVolumeN\Program Files\Waired\waired.exe`
	agentPath = `\Device\HarddiskVolumeN\Program Files\Waired\waired-agent.exe`
	bashPath  = `\Device\HarddiskVolumeN\Program Files\Git\usr\bin\bash.exe`
)

func mustParse(t *testing.T, xml string) []Record {
	t.Helper()
	recs, ok := parseRecords([]byte(xml), time.Time{})
	if !ok {
		t.Fatal("parseRecords reported it could not read the output")
	}
	return recs
}

// TestParseReadsTheNamedFields is the base case: the layout a host with the
// provider manifest registered produces.
func TestParseReadsTheNamedFields(t *testing.T) {
	recs := mustParse(t, blockXML("2026-09-04T18:43:24.1234567Z", cliPath, bashPath))
	if len(recs) != 1 {
		t.Fatalf("got %d records, want 1", len(recs))
	}
	got := recs[0]
	if got.ID != EventEnforced {
		t.Errorf("ID = %d, want %d", got.ID, EventEnforced)
	}
	if got.Program != "waired.exe" {
		t.Errorf("Program = %q, want waired.exe", got.Program)
	}
	if got.Requester != "bash.exe" {
		t.Errorf("Requester = %q, want bash.exe (the shell Claude Code runs hooks through)", got.Requester)
	}
	if !got.When.Equal(time.Date(2026, 9, 4, 18, 43, 24, 123456700, time.UTC)) {
		t.Errorf("When = %v", got.When)
	}
}

// TestParseFallsBackToPositionalData covers a host where the provider manifest
// is not registered: wevtutil emits bare <Data> elements and the only
// unambiguous thing in the array is the shape of a device path.
func TestParseFallsBackToPositionalData(t *testing.T) {
	xml := `<Event><System><EventID>3033</EventID>` +
		`<TimeCreated SystemTime='2026-09-04T17:12:54Z'/></System><EventData>` +
		`<Data>61</Data>` +
		`<Data>` + cliPath + `</Data>` +
		`<Data>59</Data>` +
		`<Data>` + bashPath + `</Data>` +
		`<Data>0xc0e90002</Data>` +
		`</EventData></Event>`
	recs := mustParse(t, xml)
	if len(recs) != 1 {
		t.Fatalf("got %d records, want 1", len(recs))
	}
	if recs[0].Program != "waired.exe" || recs[0].Requester != "bash.exe" {
		t.Errorf("got program=%q requester=%q, want waired.exe / bash.exe", recs[0].Program, recs[0].Requester)
	}
}

// TestParseIgnoresOtherPeoplesFiles. The channel records every refusal on the
// machine, and most of them are nothing to do with us.
func TestParseIgnoresOtherPeoplesFiles(t *testing.T) {
	xml := blockXML("2026-09-04T18:00:00Z", `\Device\HarddiskVolumeN\Users\x\Downloads\something.exe`, bashPath) +
		blockXML("2026-09-04T18:01:00Z", cliPath, bashPath)
	recs := mustParse(t, xml)
	if len(recs) != 1 {
		t.Fatalf("got %d records, want 1 (only ours)", len(recs))
	}
	if recs[0].Program != "waired.exe" {
		t.Errorf("kept %q", recs[0].Program)
	}
}

// TestParseDropsRecordsFromBeforeTheCutoff. A refusal that ended before this
// boot is history.
func TestParseDropsRecordsFromBeforeTheCutoff(t *testing.T) {
	cutoff := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	xml := blockXML("2026-09-03T09:00:00Z", cliPath, bashPath) +
		blockXML("2026-09-04T18:00:00Z", cliPath, bashPath)
	recs, ok := parseRecords([]byte(xml), cutoff)
	if !ok {
		t.Fatal("parseRecords reported it could not read the output")
	}
	if len(recs) != 1 {
		t.Fatalf("got %d records, want 1 (the one after the cutoff)", len(recs))
	}
}

// TestParseReportsUnreadableOutput. The caller turns this into "say nothing",
// which must never be reachable from a successful read of an empty log.
func TestParseReportsUnreadableOutput(t *testing.T) {
	if _, ok := parseRecords([]byte("<Event><System><EventID>3077"), time.Time{}); ok {
		t.Error("truncated XML parsed as a successful read")
	}
	recs, ok := parseRecords(nil, time.Time{})
	if !ok {
		t.Error("empty output should be a successful read of nothing, not a failure")
	}
	if len(recs) != 0 {
		t.Errorf("got %d records from empty output", len(recs))
	}
}

// TestExplainGroupsRefusalsByProgram is the shape of the #1217 host: one file
// refused many times by one requester, while the other two ran fine.
func TestExplainGroupsRefusalsByProgram(t *testing.T) {
	var xml strings.Builder
	base := time.Date(2026, 9, 4, 17, 12, 54, 0, time.UTC)
	for i := 0; i < 234; i++ {
		xml.WriteString(blockXML(base.Add(time.Duration(i)*23*time.Second).Format(time.RFC3339), cliPath, bashPath))
	}
	res := Explain("windows", mustParse(t, xml.String()))

	if res.Status != Refused {
		t.Fatalf("Status = %v, want Refused", res.Status)
	}
	if len(res.Refusals) != 1 {
		t.Fatalf("got %d refused programs, want 1", len(res.Refusals))
	}
	r := res.Refusals[0]
	if r.Program != "waired.exe" || r.Count != 234 {
		t.Errorf("got %s x%d, want waired.exe x234", r.Program, r.Count)
	}
	if len(r.Requesters) != 1 || r.Requesters[0] != "bash.exe" {
		t.Errorf("Requesters = %v, want one bash.exe -- repeats must collapse", r.Requesters)
	}
	if !res.Refused("waired.exe") {
		t.Error("Refused(waired.exe) = false")
	}
	if res.Refused("waired-agent.exe") {
		t.Error("Refused(waired-agent.exe) = true, but nothing refused it")
	}
}

// TestExplainSaysNothingOffWindows. Every other OS returns NotApplicable, and
// the untagged core has to agree with the stub collector.
func TestExplainSaysNothingOffWindows(t *testing.T) {
	for _, goos := range []string{"linux", "darwin", "freebsd"} {
		res := Explain(goos, mustParse(t, blockXML("2026-09-04T18:00:00Z", cliPath, bashPath)))
		if res.Status != NotApplicable {
			t.Errorf("%s: Status = %v, want NotApplicable", goos, res.Status)
		}
		if res.Cause() != "" {
			t.Errorf("%s: Cause = %q, want empty", goos, res.Cause())
		}
	}
}

// TestExplainClearWhenNothingWasRefused. Reading the log and finding nothing
// is a real answer, distinct from not being able to read it.
func TestExplainClearWhenNothingWasRefused(t *testing.T) {
	res := Explain("windows", nil)
	if res.Status != Clear {
		t.Errorf("Status = %v, want Clear", res.Status)
	}
	if res.Cause() != "" || res.Hint() != "" {
		t.Errorf("Clear should say nothing, got cause=%q hint=%q", res.Cause(), res.Hint())
	}
}

// TestExplainReadsTheReputationAnswer. Event 3118 is what separates "the cloud
// was asked and had no trust value" from "this device's cache answered". Both
// happened in the measured run: the first refusal carried the call, the rest
// were answered from the cache it filled.
func TestExplainReadsTheReputationAnswer(t *testing.T) {
	cached := Explain("windows", mustParse(t,
		blockXML("2026-09-04T18:43:24Z", cliPath, bashPath)+
			detailXML("2026-09-04T18:43:24Z", cliPath, "0x0")))
	if len(cached.Refusals) != 1 {
		t.Fatalf("got %d refusals", len(cached.Refusals))
	}
	if !cached.Refusals[0].AnsweredFromCache || cached.Refusals[0].AskedTheCloud {
		t.Errorf("0x0 should read as cache: cache=%v cloud=%v",
			cached.Refusals[0].AnsweredFromCache, cached.Refusals[0].AskedTheCloud)
	}

	asked := Explain("windows", mustParse(t,
		blockXML("2026-09-04T17:12:54Z", cliPath, bashPath)+
			detailXML("2026-09-04T17:12:54Z", cliPath, "0xc8000000")))
	if !asked.Refusals[0].AskedTheCloud || asked.Refusals[0].AnsweredFromCache {
		t.Errorf("0xc8000000 should read as a cloud call: cache=%v cloud=%v",
			asked.Refusals[0].AnsweredFromCache, asked.Refusals[0].AskedTheCloud)
	}

	// No 3118 at all: neither is known, and neither may be claimed.
	none := Explain("windows", mustParse(t, blockXML("2026-09-04T18:00:00Z", cliPath, bashPath)))
	if none.Refusals[0].AskedTheCloud || none.Refusals[0].AnsweredFromCache {
		t.Error("with no block-detail record, both answers must stay unknown")
	}
}

// TestExplainIgnoresADetailWithNoRefusal. A 3118 on its own does not make a
// refusal: the package reports what the log says, not what it implies.
func TestExplainIgnoresADetailWithNoRefusal(t *testing.T) {
	res := Explain("windows", mustParse(t, detailXML("2026-09-04T18:00:00Z", cliPath, "0x0")))
	if res.Status != Clear {
		t.Errorf("Status = %v, want Clear -- a block detail alone is not a block", res.Status)
	}
}

func TestIsZeroField(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want bool
	}{
		{"0", true}, {"0x0", true}, {"0x00000000", true}, {"0X0", true},
		{"0xc8000000", false}, {"1", false}, {"", false}, {"   ", false},
	} {
		if got := isZeroField(tc.in); got != tc.want {
			t.Errorf("isZeroField(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestMatchProgram(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want string
	}{
		{cliPath, "waired.exe"},
		{agentPath, "waired-agent.exe"},
		{`\??\C:\Program Files\Waired\WAIRED-TRAY.EXE`, "waired-tray.exe"},
		// A file whose name merely contains ours is not ours.
		{`\Device\HarddiskVolumeN\tmp\not-waired.exe`, ""},
		{`\Device\HarddiskVolumeN\tmp\waired.exe.bak`, ""},
		{"", ""},
	} {
		if got := MatchProgram(tc.in); got != tc.want {
			t.Errorf("MatchProgram(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestCauseNamesTheFileAndWhatStillWorks. The message has one job beyond
// naming the file: telling the reader the machine is not broken. That is only
// sayable because the verdict is per file, and it is what the #1217 host
// demonstrated — the service and the app ran throughout.
func TestCauseNamesTheFileAndWhatStillWorks(t *testing.T) {
	res := Explain("windows", mustParse(t,
		blockXML("2026-09-04T17:12:54Z", cliPath, bashPath)+
			blockXML("2026-09-04T18:43:24Z", cliPath, bashPath)))
	cause := res.Cause()
	for _, want := range []string{
		"waired.exe",
		"waired-agent.exe", // named as unaffected
		"waired-tray.exe",  // ditto
		"per file",
		"lift on its own",
		"2 refused launches",
		"over 90 minutes",
	} {
		if !strings.Contains(cause, want) {
			t.Errorf("Cause() does not mention %q:\n%s", want, cause)
		}
	}

	hint := res.Hint()
	if !strings.Contains(hint, "status line") {
		t.Errorf("with waired.exe refused, the hint must say why Claude Code went quiet:\n%s", hint)
	}
	if !strings.Contains(hint, "759") {
		t.Errorf("the hint must point at the permanent fix:\n%s", hint)
	}
}

// TestCauseDoesNotBlameClaudeCodeForTheWrongFile. Only a refused waired.exe
// silences the status line and the hooks; a refused tray says nothing about
// them.
func TestCauseDoesNotBlameClaudeCodeForTheWrongFile(t *testing.T) {
	trayPath := `\Device\HarddiskVolumeN\Program Files\Waired\waired-tray.exe`
	res := Explain("windows", mustParse(t, blockXML("2026-09-04T18:00:00Z", trayPath, bashPath)))
	if strings.Contains(res.Hint(), "status line") {
		t.Errorf("a refused tray must not be reported as breaking Claude Code:\n%s", res.Hint())
	}
	if !strings.Contains(res.Cause(), "waired.exe is unaffected") &&
		!strings.Contains(res.Cause(), "waired.exe and waired-agent.exe are unaffected") {
		t.Errorf("Cause() should name the files that still work:\n%s", res.Cause())
	}
}

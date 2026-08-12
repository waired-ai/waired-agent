// Package servicediag explains why the waired-agent service is not running.
//
// `waired doctor` could already tell you the daemon was unreachable. It could
// not tell you why, and the why is where the useful part lives: during the
// 0.0.2-rc7 review a Windows host came back from a reboot with the service
// stopped, and the answer — Smart App Control had blocked the unsigned binary
// at boot — was only available to someone who knew to correlate SCM event 7000
// with CodeIntegrity 3033 in Event Viewer. Nobody diagnoses that from
// "unreachable" (#315).
//
// Shape follows internal/platform/trayhost: a per-OS Collect gathers raw
// evidence, and an untagged Explain turns it into a verdict. Explain takes the
// GOOS rather than reading runtime.GOOS so every platform's interpretation is
// table-tested on every CI leg (CLAUDE.md §Test discipline) — which matters
// most here, because the Windows evidence this decodes cannot be produced on
// the Linux leg at all.
package servicediag

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/waired-ai/waired-agent/internal/platform/logrotate"
	"github.com/waired-ai/waired-agent/internal/platform/service"
)

// Status is the verdict.
type Status int

const (
	// Unknown means we could not gather evidence — the log is unreadable, the
	// tool is missing, or the OS has no service backend. Callers emit nothing:
	// a doctor line that says "could not tell" is noise.
	Unknown Status = iota
	// Healthy means the service is running and nothing recent looks wrong.
	Healthy
	// Failed means the service is not running and we know why.
	Failed
	// Stopped means the service is not running and nothing failed — someone
	// or something stopped it deliberately.
	Stopped
)

// Event is one line of platform evidence: a Windows event-log record, a
// systemd property, a journal entry, a launchd exit status.
type Event struct {
	// Source is the provider or subsystem ("Service Control Manager",
	// "Microsoft-Windows-CodeIntegrity", "systemd", "launchd").
	Source string
	// ID is the numeric event ID where the platform has one (Windows), 0
	// elsewhere.
	ID int
	// Message is the raw text, already trimmed.
	Message string
	// When is the time the platform recorded for this record, where it
	// has one (Windows). Zero on the Unix collectors, whose evidence is
	// current-state properties rather than dated records — recentEvents
	// keeps zero-valued entries for exactly that reason.
	When time.Time
}

// EventField is one EventData entry from a Windows event record. Name is
// the provider manifest's name for it ("FileNameBuffer") and is empty when
// the manifest is not installed, which is why summarizeCodeIntegrity has a
// positional fallback.
type EventField struct {
	Name  string
	Value string
}

// recentEvents drops dated records older than cutoff.
//
// The Windows service check had no time window at all: it took the 20
// newest matching records and let the first one explain the current state.
// On the rc8 host that surfaced a CodeIntegrity block from 2026-06-29 —
// six weeks and several reinstalls of the service earlier — as the reason
// a service that was running fine had a ⚠ next to it (#653).
//
// The cutoff callers pass is the current boot. A record written before the
// machine booted cannot describe the state of a service that has only
// existed since it did, and "blocked at boot" is the case the whole check
// was written for (#315), so the window covers it exactly.
//
// Entries with no timestamp are kept: the Unix collectors report
// current-state properties, which are always about now.
func recentEvents(events []Event, cutoff time.Time) []Event {
	out := events[:0:0]
	for _, e := range events {
		if !e.When.IsZero() && e.When.Before(cutoff) {
			continue
		}
		out = append(out, e)
	}
	return out
}

// summarizeCodeIntegrity turns a CodeIntegrity record's EventData into one
// readable clause.
//
// The collector used to join the whole positional array with spaces, so
// the doctor printed length prefixes, two device paths, four hashes, a
// policy GUID and a string of zeroes into the middle of a sentence. The
// prose around it was fine; the tail swamped the terminal (#653).
//
// Everything an operator can act on is the blocked file and, when the
// record names one, the process that tried to load it. Hashes, policy
// GUIDs and signing-level codes are for someone reading Event Viewer, and
// that person has the record itself.
//
// Named fields are preferred. The positional fallback exists because
// wevtutil emits bare <Data> elements when the provider manifest is not
// registered, and it keys on the one thing that is unambiguous in that
// array: a value shaped like an NT device path.
func summarizeCodeIntegrity(fields []EventField) string {
	byName := make(map[string]string, len(fields))
	var paths []string
	for _, f := range fields {
		if f.Name != "" {
			byName[f.Name] = f.Value
		}
		if looksLikeNTPath(f.Value) {
			paths = append(paths, f.Value)
		}
	}

	blocked := byName["FileNameBuffer"]
	loader := byName["ProcessNameBuffer"]
	if blocked == "" && len(paths) > 0 {
		// Positional layout: the blocked file is named before the process
		// that requested it.
		blocked = paths[0]
		if loader == "" && len(paths) > 1 {
			loader = paths[1]
		}
	}
	switch {
	case blocked == "":
		return ""
	case loader == "":
		return "blocked " + blocked
	default:
		return "blocked " + blocked + ", requested by " + loader
	}
}

// eventMessage picks what the doctor quotes for one Windows record.
//
// CodeIntegrity records are summarised: their EventData is a positional
// array of length prefixes, device paths, four hashes, a policy GUID and
// trailing zeroes, and joining it verbatim is what produced the unreadable
// bracketed tail in #653. SCM records already carry prose, so they pass
// through. A summary that comes back empty (an unrecognised layout) falls
// back to the raw join rather than dropping the evidence entirely.
//
// It lives in the untagged file, not next to its only caller in
// collect_windows.go, so it runs on every CI leg — the Windows evidence
// this decodes cannot be produced on the Linux leg at all, which is the
// same reason Explain takes a goos instead of reading runtime.GOOS.
func eventMessage(id int, fields []EventField, raw string) string {
	switch id {
	case winCodeIntegrityBlocked, winCodeIntegrityAudit:
		if s := summarizeCodeIntegrity(fields); s != "" {
			return s
		}
	}
	return raw
}

// parseSystemTime reads a Windows event's RFC3339 timestamp. An
// unparseable or absent stamp yields the zero time, which recentEvents
// treats as always relevant — dropping a record because its clock is
// unreadable would lose the diagnosis this package exists to make.
func parseSystemTime(v string) time.Time {
	if v == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339Nano, v)
	if err != nil {
		return time.Time{}
	}
	return t
}

// looksLikeNTPath recognises the \Device\HarddiskVolumeN\... form the
// CodeIntegrity provider writes file names in. Deliberately narrow: the
// point is to pick paths out of an array that also holds hashes and
// version strings, not to validate a path.
func looksLikeNTPath(v string) bool {
	return strings.HasPrefix(v, `\Device\`) || strings.HasPrefix(v, `\??\`)
}

// Result is what Explain decides.
type Result struct {
	Status Status
	// Cause is a plain-language sentence naming what happened. Empty when
	// Status is Unknown.
	Cause string
	// Hint is what to do about it, or "" when there is nothing to suggest.
	Hint string
	// Evidence is the raw line the verdict rests on, so `waired doctor` can
	// show its work rather than asking to be trusted.
	Evidence string
}

// Windows Service Control Manager event IDs. Deliberately not 7036 (the
// generic "entered the running/stopped state" pair): the rc7 host's System log
// contained no 7036 records at all for the day in question, so a decoder that
// keys on it reports "nothing happened" on exactly the machine that failed.
const (
	winSCMStartFailed  = 7000 // service failed to start (carries the reason)
	winSCMStartTimeout = 7009 // timed out connecting to the service
	winSCMStartHung    = 7022 // service hung on starting
	winSCMLoginFailed  = 7023 // service terminated with an error
	// winSCMSpecificExit is "terminated with the following service-specific
	// error". It is the only SCM record that carries the status the service
	// itself chose, which is what tells a deliberate restart request apart
	// from a crash — the pair 7031/7034 describes both identically (#727).
	winSCMSpecificExit = 7024
	winSCMTerminated   = 7031 // service terminated unexpectedly
	winSCMActionTaken  = 7034 // service terminated unexpectedly, no recovery left
)

// serviceSpecificExit reads the exit status out of an SCM 7024 message.
//
// 7024's EventData is the service's display name followed by the status it
// exited with, and queryEvents joins those values into Message — so the
// status is the last integer token. The SCM sometimes writes it with the
// `%%` prefix it uses for message-table ids, so that prefix is accepted
// too.
//
// A record that does not decode yields (0, false), and the caller treats
// that as "no information" rather than as "not a failure": reading an
// undecodable stop as benign is the one mistake that would hide a real
// crash.
func serviceSpecificExit(msg string) (int, bool) {
	fields := strings.Fields(msg)
	for i := len(fields) - 1; i >= 0; i-- {
		tok := strings.TrimPrefix(fields[i], "%%")
		if n, err := strconv.Atoi(tok); err == nil && n > 0 {
			return n, true
		}
	}
	return 0, false
}

// Windows CodeIntegrity event IDs: a binary was refused for not meeting the
// signing level. 3077 is the audit-mode twin of 3033.
const (
	winCodeIntegrityBlocked = 3033
	winCodeIntegrityAudit   = 3077
)

// Explain turns collected evidence into a verdict.
//
// running is what the platform says about the service right now; the events
// are the recent history. A running service still gets its history read,
// because "it is up now but it was blocked at boot and only started when you
// logged in and clicked" is a different — and more useful — answer than "OK".
//
// stateDir is only used to name the agent's log file in a hint, and only
// on the platforms that have one; "" simply omits the pointer.
func Explain(goos string, running bool, events []Event, stateDir string) Result {
	switch goos {
	case "windows":
		return explainWindows(running, events, stateDir)
	case "linux":
		return explainLinux(running, events)
	case "darwin":
		return explainDarwin(running, events)
	default:
		return Result{}
	}
}

// logHint points at the agent's own log where the platform keeps one.
//
// Windows had no such pointer at all until #636 gave it a log file: the
// Event Log takes WARN and above, so every INFO diagnostic the agent
// writes was unreachable there, and this check could only say "start it
// again". Now that there is a file, the hint names it — through
// logrotate.AgentLogPath, the single definition every surface shares,
// rather than spelling the path a second time here.
//
// Empty on Linux, where there is no file: journalctl is already named in
// the Linux hints.
func logHint(goos, stateDir string) string {
	path := logrotate.AgentLogPath(goos, stateDir)
	if path == "" {
		return ""
	}
	return " The agent's own log is at " + path + "."
}

func explainWindows(running bool, events []Event, stateDir string) Result {
	log := logHint("windows", stateDir)
	blocked := findEvent(events, winCodeIntegrityBlocked, winCodeIntegrityAudit)
	startFailed := findEvent(events, winSCMStartFailed)

	// The rc7 case. SCM 7000 says the start failed; the CodeIntegrity record
	// in the same second says why. Either alone is decodable — 7000's own
	// message carries "An Application Control policy has blocked this file" —
	// but the pair is unambiguous, so check it first.
	policyBlocked := blocked != nil ||
		(startFailed != nil && mentionsPolicyBlock(startFailed.Message))
	if policyBlocked {
		ev := startFailed
		if ev == nil {
			ev = blocked
		}
		return Result{
			Status: statusFor(running, Failed),
			Cause: "Windows blocked the Waired background service at startup because " +
				"Waired's programs are not signed with a certificate Windows recognises " +
				"(Smart App Control / an application-control policy).",
			Hint: "Start it from the Waired menu, or with `" + startCommandWindows + "` " +
				"from an administrator PowerShell. This can recur at boot until Waired ships signed programs." + log,
			Evidence: ev.line(),
		}
	}

	if startFailed != nil {
		return Result{
			Status:   statusFor(running, Failed),
			Cause:    "The Waired background service failed to start.",
			Hint:     "Start it from the Waired menu; if it fails again, the message below names the reason." + log,
			Evidence: startFailed.line(),
		}
	}
	if ev := findEvent(events, winSCMStartTimeout, winSCMStartHung); ev != nil {
		return Result{
			Status:   statusFor(running, Failed),
			Cause:    "The Waired background service took too long to start and Windows gave up on it.",
			Hint:     "Start it from the Waired menu. If it keeps timing out, run `waired logs` and report it." + log,
			Evidence: ev.line(),
		}
	}
	// Checked BEFORE the unexpected-stop branch below, because a
	// restart-me exit produces both: 7024 records the status the agent
	// chose, and 7031/7034 follow as the SCM's recovery action. Only 7024
	// can tell the two apart — 7031 says "terminated unexpectedly" about a
	// deliberate restart just as readily as about a crash (#727).
	//
	// A 7024 carrying any OTHER status is left to fall through: it is a
	// genuine failure, and the existing branches already describe it. Only
	// the restart request is subtracted.
	if ev := findEvent(events, winSCMSpecificExit); ev != nil {
		if code, ok := serviceSpecificExit(ev.Message); ok && code == service.RestartRequestedExitCode {
			return quietResult(running)
		}
	}
	if ev := findEvent(events, winSCMTerminated, winSCMActionTaken, winSCMLoginFailed); ev != nil {
		return Result{
			Status:   statusFor(running, Failed),
			Cause:    "The Waired background service stopped unexpectedly.",
			Hint:     "Windows restarts it automatically; if it stays down, start it from the Waired menu." + log,
			Evidence: ev.line(),
		}
	}
	return quietResult(running)
}

func explainLinux(running bool, events []Event) Result {
	// systemctl show gives the authoritative post-mortem; the journal lines
	// are only there to quote back.
	result := property(events, "Result")
	switch result {
	case "exit-code", "core-dump", "signal":
		return Result{
			Status: statusFor(running, Failed),
			Cause: "The Waired background service exited with an error" +
				restartSuffix(property(events, "NRestarts")) + ".",
			Hint:     "Run `journalctl -u waired-agent -b` for the full log, or start it from the Waired menu.",
			Evidence: evidenceOr(events, "systemd: Result="+result),
		}
	case "timeout":
		return Result{
			Status:   statusFor(running, Failed),
			Cause:    "The Waired background service timed out while starting.",
			Hint:     "Run `journalctl -u waired-agent -b` for the full log.",
			Evidence: evidenceOr(events, "systemd: Result=timeout"),
		}
	}
	if property(events, "ActiveState") == "inactive" && result == "success" {
		return Result{
			Status:   Stopped,
			Cause:    "The Waired background service is stopped. Nothing failed — it was stopped deliberately.",
			Hint:     "Start it from the Waired menu, or with `" + startCommandLinux + "`.",
			Evidence: "systemd: ActiveState=inactive Result=success",
		}
	}
	return quietResult(running)
}

func explainDarwin(running bool, events []Event) Result {
	// launchd reports the previous run's exit status; a nonzero one is the
	// daemon having died rather than been stopped.
	//
	// It has to be read as a NUMBER, not merely as "not the string 0".
	// launchd's healthy steady state prints `last exit code = (never
	// exited)`, and a string compare let that through into the "exited with
	// an error (status (never exited))" sentence — a warning that
	// contradicts itself, on a host whose service the same doctor run
	// reported as up and serving (#652). A value that is not a number is
	// not evidence of anything, so it falls through to quietResult.
	// ...and exit 17 is not "having died" at all: it is the agent asking to
	// be restarted (service.RestartRequestedExitCode). Two paths take it
	// today: the wedged-engine self-heal (cmd/waired-agent/inference.go's
	// restartOnWedge) and /preferred-model's restart FALLBACK — reached
	// when the in-process swap cannot apply the switch (wedged engine,
	// cross-engine, unenrolled). A plain switch between two models of the
	// same engine no longer restarts anything, so this is rarer than
	// #727's report assumed; it is not gone. launchd has no per-exit-code
	// key to learn the intent from — the
	// #721 constraint — but that constraint is about whether launchd will
	// RESTART the agent, and this code only has to EXPLAIN a stop that has
	// already happened. By the time doctor runs, the supervisor has brought
	// the agent back, so the record was being read out as
	// "⚠ exited with an error (status 17)" on a service that is up and
	// working (#727).
	if code, ok := exitCode(property(events, "last exit code")); ok && code != 0 && code != service.RestartRequestedExitCode {
		return Result{
			Status:   statusFor(running, Failed),
			Cause:    fmt.Sprintf("The Waired background service exited with an error (status %d).", code),
			Hint:     "Check " + logrotate.AgentLogPath("darwin", "") + ", or start it from the Waired menu.",
			Evidence: evidenceOr(events, fmt.Sprintf("launchd: last exit code = %d", code)),
		}
	}
	if property(events, "state") == "not running" {
		return Result{
			Status:   Stopped,
			Cause:    "The Waired background service is not running.",
			Hint:     "Start it from the Waired menu, or with `" + startCommandDarwin + "`.",
			Evidence: "launchd: state = not running",
		}
	}
	return quietResult(running)
}

// The commands quoted in hints. Duplicated from service.StartHint rather than
// imported so this package stays free of the service package's SCM/systemd
// dependencies; the agreement is pinned by a test.
const (
	startCommandWindows = "Start-Service waired-agent"
	startCommandLinux   = "sudo systemctl start waired-agent"
	startCommandDarwin  = "sudo launchctl kickstart -k system/com.waired.agent"
)

// statusFor keeps a recovered service honest: the history explains something
// that already got fixed, so report Healthy and let the caller decide whether
// a past failure is worth mentioning.
func statusFor(running bool, ifDown Status) Status {
	if running {
		return Healthy
	}
	return ifDown
}

func quietResult(running bool) Result {
	if running {
		return Result{Status: Healthy, Cause: "The Waired background service is running."}
	}
	// Down, with nothing in the evidence to explain it. Saying "I don't know"
	// is worse than saying nothing: the caller already reports the daemon as
	// unreachable.
	return Result{}
}

// exitCode reads a service manager's "last exit code" value as a number.
//
// launchctl prints `(never exited)` there for a job that has been up since
// it was loaded — the healthy steady state — and other non-numeric words
// for states that are equally not an exit status. Only a number is
// evidence about how the last run ended, so anything else reports false
// and the caller stays quiet (#652).
func exitCode(v string) (int, bool) {
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil {
		return 0, false
	}
	return n, true
}

func restartSuffix(nRestarts string) string {
	if nRestarts == "" || nRestarts == "0" {
		return ""
	}
	return fmt.Sprintf(" and has been restarted %s time(s)", nRestarts)
}

// findEvent returns the first event matching any of the given IDs.
func findEvent(events []Event, ids ...int) *Event {
	for i := range events {
		for _, id := range ids {
			if events[i].ID == id {
				return &events[i]
			}
		}
	}
	return nil
}

// property reads a `key: value` or `key=value` event carrying no ID — how the
// Unix collectors report systemctl / launchctl output. Matching is
// case-insensitive because launchctl's key casing varies by release.
func property(events []Event, key string) string {
	want := strings.ToLower(key)
	for _, e := range events {
		if e.ID != 0 {
			continue
		}
		for _, sep := range []string{"=", ":"} {
			k, v, ok := strings.Cut(e.Message, sep)
			if !ok {
				continue
			}
			if strings.ToLower(strings.TrimSpace(k)) == want {
				return strings.TrimSpace(v)
			}
		}
	}
	return ""
}

// evidenceOr quotes the first log line, falling back to a synthesised summary
// when the collector produced only properties.
func evidenceOr(events []Event, fallback string) string {
	for _, e := range events {
		if e.ID == 0 && strings.Contains(e.Message, "=") {
			continue // a property, not a log line
		}
		if e.Message != "" {
			return e.line()
		}
	}
	return fallback
}

func (e Event) line() string {
	src := e.Source
	if src != "" && !e.When.IsZero() {
		// Minute precision, local time. The provider hands over an
		// RFC3339 stamp with sub-second digits and a zone offset, which
		// is more precision than anyone reading a doctor line needs and
		// was part of what made the old evidence tail unreadable (#653).
		src = fmt.Sprintf("%s (%s)", src, e.When.Local().Format("2006-01-02 15:04"))
	}
	switch {
	case src != "" && e.ID != 0:
		return fmt.Sprintf("%s event %d: %s", src, e.ID, e.Message)
	case src != "":
		return src + ": " + e.Message
	default:
		return e.Message
	}
}

// mentionsPolicyBlock recognises the SCM 7000 reason text for an
// application-control block, in the two shapes Windows renders it.
func mentionsPolicyBlock(msg string) bool {
	m := strings.ToLower(msg)
	return strings.Contains(m, "application control policy") ||
		strings.Contains(m, "blocked by policy") ||
		strings.Contains(m, "code integrity")
}

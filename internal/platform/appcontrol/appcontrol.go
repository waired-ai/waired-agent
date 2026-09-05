// Package appcontrol reports which of Waired's own programs Windows is
// currently refusing to run, and says so on a surface that is still working.
//
// Windows Application Control — Smart App Control on consumer Windows 11, or a
// WDAC policy elsewhere — decides per FILE whether an unsigned executable may
// start, and the decision moves on its own. Two files out of one archive land
// on opposite sides, and a file refused this morning runs this afternoon with
// nothing changed on the machine
// (docs/knowledges/20260829/1740-sac-verdict-is-per-file-and-moves.md).
//
// The consequence that made this package necessary: on a host where the
// verdict had gone against waired.exe, Claude Code's Waired status line and
// hooks kept invoking it and Windows kept refusing — 234 launches in 91
// minutes — and nothing anywhere told the user why the integration had gone
// quiet (waired-agent#1217). `waired doctor` could not say it either, because
// `waired doctor` IS waired.exe. What was still running throughout was the
// background service and the Waired app, which are separate files with their
// own verdicts. So the reading is done here, in a package both can import, and
// the tray — the surface that survives a refused CLI — is the one that speaks.
//
// This is a different question from internal/platform/servicediag's. That
// package asks "why is the service down", reads the same log scoped to
// waired-agent.exe, and says nothing at all while the service is healthy. On
// the #1217 host the service was healthy the whole time.
//
// Check is per-OS: the real reader is collect_windows.go. Everywhere else
// returns NotApplicable — no other OS we ship to refuses to execute a file on
// a reputation verdict.
package appcontrol

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// Programs are the executables Windows judges separately. One list, because
// the reader, the prose and the tests must not disagree about what "Waired's
// programs" means — servicediag's CodeIntegrity query was scoped to
// waired-agent.exe alone, which is exactly why 234 refusals of waired.exe
// produced no evidence anywhere (waired-agent#1217).
var Programs = []string{"waired.exe", "waired-agent.exe", "waired-tray.exe"}

// Status is the verdict of Check.
type Status int

const (
	// NotApplicable means the question does not apply here (not Windows), or
	// the log could not be read. Callers emit nothing.
	NotApplicable Status = iota
	// Clear means the log was read and holds no refusal of a Waired program
	// in the window looked at.
	Clear
	// Refused means Windows refused at least one Waired program.
	Refused
)

func (s Status) String() string {
	switch s {
	case Clear:
		return "clear"
	case Refused:
		return "refused"
	default:
		return "not-applicable"
	}
}

// Record is one CodeIntegrity event, already parsed. Keeping the collector's
// output as data — rather than having it build sentences — is what lets the
// grouping and the prose below be table-tested on every OS.
type Record struct {
	// ID is the CodeIntegrity event ID: 3033 (blocked) or 3077 (blocked under
	// an enforcement policy) for a refusal, 3118 for the block details that
	// follow one.
	ID int
	// When the record was written.
	When time.Time
	// Program is the Waired executable the record is about, matched against
	// Programs; "" when the record names none of them.
	Program string
	// Requester is the process that tried to start it, if the record names
	// one (services.exe for a service start, bash.exe for a Claude Code hook).
	Requester string
	// Fields is the record's EventData by name, as read. Field sets differ
	// between Windows builds, so nothing here may be assumed present
	// (docs/knowledges/20260904/0300-the-reputation-verdict-is-in-event-3118.md).
	Fields map[string]string
}

// Event IDs. 3118 carries the reputation answer and was read by nothing in
// this repository before waired-agent#1191 looked for it.
const (
	EventBlocked     = 3033
	EventEnforced    = 3077
	EventBlockDetail = 3118
)

func isRefusal(id int) bool { return id == EventBlocked || id == EventEnforced }

// Refusal is what happened to one program.
type Refusal struct {
	Program    string
	Count      int
	First      time.Time
	Last       time.Time
	Requesters []string
	// AskedTheCloud is true when a block detail record reported an HTTP status
	// for the reputation lookup, i.e. the verdict came from a live call rather
	// than this device's cache. Both were measured on the #1217 host: the
	// first refusal of a run carried the call, and the 233 after it were
	// answered from the cache it filled.
	AskedTheCloud bool
	// AnsweredFromCache is true when a block detail record reported no cloud
	// call. It is not simply !AskedTheCloud: with no 3118 record at all,
	// neither is known and both stay false.
	AnsweredFromCache bool
}

// Result is what Check found.
type Result struct {
	Status Status
	// Refusals holds one entry per refused program, most recently refused
	// first. Empty unless Status is Refused.
	Refusals []Refusal
}

// Refused reports whether a particular program was refused.
func (r Result) Refused(program string) bool {
	for _, x := range r.Refusals {
		if strings.EqualFold(x.Program, program) {
			return true
		}
	}
	return false
}

// Explain turns raw records into a Result. Untagged and pure so the grouping
// can be tested on any OS against captured event data.
//
// records are expected newest-first, which is how the collector asks for them;
// Explain does not depend on that beyond choosing First/Last by comparison.
func Explain(goos string, records []Record) Result {
	if goos != "windows" {
		return Result{Status: NotApplicable}
	}

	byProgram := map[string]*Refusal{}
	seenRequester := map[string]map[string]bool{}
	for _, rec := range records {
		if rec.Program == "" || !isRefusal(rec.ID) {
			continue
		}
		key := strings.ToLower(rec.Program)
		cur := byProgram[key]
		if cur == nil {
			cur = &Refusal{Program: rec.Program}
			byProgram[key] = cur
			seenRequester[key] = map[string]bool{}
		}
		cur.Count++
		if !rec.When.IsZero() {
			if cur.First.IsZero() || rec.When.Before(cur.First) {
				cur.First = rec.When
			}
			if rec.When.After(cur.Last) {
				cur.Last = rec.When
			}
		}
		if rec.Requester != "" && !seenRequester[key][rec.Requester] {
			seenRequester[key][rec.Requester] = true
			cur.Requesters = append(cur.Requesters, rec.Requester)
		}
	}

	// The block details, folded onto the program they belong to. A 3118 for a
	// program that was never refused is ignored rather than inventing a
	// refusal from it.
	for _, rec := range records {
		if rec.ID != EventBlockDetail || rec.Program == "" {
			continue
		}
		cur := byProgram[strings.ToLower(rec.Program)]
		if cur == nil {
			continue
		}
		code, ok := rec.Fields["DefenderCloudHTTPCode"]
		if !ok {
			continue
		}
		if isZeroField(code) {
			cur.AnsweredFromCache = true
		} else {
			cur.AskedTheCloud = true
		}
	}

	if len(byProgram) == 0 {
		return Result{Status: Clear}
	}
	out := make([]Refusal, 0, len(byProgram))
	for _, v := range byProgram {
		out = append(out, *v)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].Last.Equal(out[j].Last) {
			return out[i].Last.After(out[j].Last)
		}
		return out[i].Program < out[j].Program
	})
	return Result{Status: Refused, Refusals: out}
}

// isZeroField reports whether a CodeIntegrity numeric field means zero. The
// provider writes them in several spellings depending on the Windows build —
// "0", "0x0", "0x00000000" — so compare on the value, not the text.
func isZeroField(s string) bool {
	s = strings.TrimSpace(strings.ToLower(s))
	s = strings.TrimPrefix(s, "0x")
	if s == "" {
		return false
	}
	return strings.Trim(s, "0") == ""
}

// Cause is the sentence that says what is happening. One implementation, so
// the tray toast and the doctor row cannot drift apart.
//
// Everything it asserts is measured: the verdict is per file and moves on its
// own (docs/knowledges/20260829/1740-sac-verdict-is-per-file-and-moves.md),
// and the programs not named here were running throughout on the host that
// prompted this (waired-agent#1217). It does not say the file is safe, or
// predict when the refusal will lift.
func (r Result) Cause() string {
	if r.Status != Refused || len(r.Refusals) == 0 {
		return ""
	}
	names := make([]string, 0, len(r.Refusals))
	for _, x := range r.Refusals {
		names = append(names, x.Program)
	}
	unaffected := r.stillRunning()

	var b strings.Builder
	b.WriteString("Windows is refusing to run ")
	b.WriteString(joinNames(names))
	b.WriteString(" on this computer")
	if n := r.total(); n > 1 {
		fmt.Fprintf(&b, " (%d refused launches", n)
		if w := r.window(); w != "" {
			b.WriteString(", " + w)
		}
		b.WriteString(")")
	}
	b.WriteString(". Waired's programs are not signed with a certificate Windows recognises, ")
	b.WriteString("so Smart App Control (or another application-control policy) can refuse to run them. ")
	b.WriteString("The refusal is per file and can lift on its own")
	if len(unaffected) > 0 {
		b.WriteString(", and ")
		b.WriteString(joinNames(unaffected))
		if len(unaffected) == 1 {
			b.WriteString(" is unaffected")
		} else {
			b.WriteString(" are unaffected")
		}
	}
	b.WriteString(".")
	return b.String()
}

// Hint says what to do, which on this defect is mostly "not what you would
// guess". There is no setting to change and no repair to run: the file is
// unsigned, and signing it is waired-ai/waired#759.
func (r Result) Hint() string {
	if r.Status != Refused || len(r.Refusals) == 0 {
		return ""
	}
	h := "There is nothing to repair here — try again later, and the same file may be accepted. "
	if r.Refused("waired.exe") {
		h += "Until it is, the `waired` command cannot start, which is also why Claude Code's Waired status line and hooks do nothing. "
	}
	return h + "Signed programs would end this for good (waired-ai/waired#759)."
}

// total is every refusal across every program.
func (r Result) total() int {
	n := 0
	for _, x := range r.Refusals {
		n += x.Count
	}
	return n
}

// window describes when the refusals happened, in whole minutes. Individual
// timestamps are left out on purpose: 234 of them is not a message, and the
// Event Log holds the detail for anyone who wants it.
func (r Result) window() string {
	var first, last time.Time
	for _, x := range r.Refusals {
		if !x.First.IsZero() && (first.IsZero() || x.First.Before(first)) {
			first = x.First
		}
		if x.Last.After(last) {
			last = x.Last
		}
	}
	if first.IsZero() || last.IsZero() {
		return ""
	}
	d := last.Sub(first)
	if d < time.Minute {
		return "within a minute"
	}
	return fmt.Sprintf("over %d minutes", int(d.Minutes()))
}

// stillRunning is the Waired programs this host has NOT refused. Naming them
// is the useful half of the message: it is what tells the reader the machine
// is not broken, and it is true because the verdict is per file.
func (r Result) stillRunning() []string {
	var out []string
	for _, p := range Programs {
		if !r.Refused(p) {
			out = append(out, p)
		}
	}
	return out
}

func joinNames(names []string) string {
	switch len(names) {
	case 0:
		return ""
	case 1:
		return names[0]
	case 2:
		return names[0] + " and " + names[1]
	default:
		return strings.Join(names[:len(names)-1], ", ") + " and " + names[len(names)-1]
	}
}

// MatchProgram returns the Waired program an NT device path names, or "" when
// it names none. Exported because the collector and its tests both need it,
// and because "which of our files is this" is the decision servicediag got
// wrong by comparing against one hardcoded name.
func MatchProgram(ntPath string) string {
	if ntPath == "" {
		return ""
	}
	lower := strings.ToLower(strings.ReplaceAll(ntPath, "/", `\`))
	base := lower
	if i := strings.LastIndex(lower, `\`); i >= 0 {
		base = lower[i+1:]
	}
	for _, p := range Programs {
		if base == strings.ToLower(p) {
			return p
		}
	}
	return ""
}

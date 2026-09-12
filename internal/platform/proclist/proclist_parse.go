package proclist

import (
	"encoding/json"
	"strconv"
	"strings"
)

// parseProcCmdline turns a Linux /proc/<pid>/cmdline blob (NUL-separated
// argv, usually with a trailing NUL) into argv. Returns nil when empty.
func parseProcCmdline(raw []byte) []string {
	if len(raw) == 0 {
		return nil
	}
	parts := strings.Split(string(raw), "\x00")
	argv := make([]string, 0, len(parts))
	for _, p := range parts {
		if p != "" {
			argv = append(argv, p)
		}
	}
	if len(argv) == 0 {
		return nil
	}
	return argv
}

// cimProc mirrors one `Get-CimInstance Win32_Process | Select
// ProcessId,CommandLine` record. CommandLine is null for processes whose
// command line the caller cannot read; those rows are dropped.
type cimProc struct {
	ProcessID   int    `json:"ProcessId"`
	CommandLine string `json:"CommandLine"`
	// ExecutablePath is Win32_Process's own answer for the program path.
	// Null for processes the caller cannot read; those decode to "" and
	// fall back to parsing argv[0] out of CommandLine.
	ExecutablePath string `json:"ExecutablePath"`
}

// parseCimJSON parses the JSON `ConvertTo-Json` produces for the CIM query.
// PowerShell emits a bare object for a single row and an array for many, so
// both shapes are accepted. Each CommandLine is tokenized with Windows
// quoting rules.
func parseCimJSON(raw []byte) ([]ProcInfo, error) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" {
		return nil, nil
	}
	var many []cimProc
	if strings.HasPrefix(trimmed, "[") {
		if err := json.Unmarshal([]byte(trimmed), &many); err != nil {
			return nil, err
		}
	} else {
		var one cimProc
		if err := json.Unmarshal([]byte(trimmed), &one); err != nil {
			return nil, err
		}
		many = []cimProc{one}
	}
	out := make([]ProcInfo, 0, len(many))
	for _, p := range many {
		if p.CommandLine == "" {
			continue
		}
		argv := argvWithProgram(p.ExecutablePath, p.CommandLine, splitWindowsCmdline)
		if len(argv) == 0 {
			continue
		}
		out = append(out, ProcInfo{PID: p.ProcessID, Argv: argv, Program: p.ExecutablePath})
	}
	return out, nil
}

// splitWindowsCmdline tokenizes a Windows command-line string into argv,
// honoring double-quoted segments (enough to keep a quoted, space-bearing
// program path intact; the numeric -np/-c flags this package reads are
// never quoted). Backslash-escaping of quotes is not modeled — it does not
// occur in Ollama runner command lines.
func splitWindowsCmdline(s string) []string {
	var argv []string
	var cur strings.Builder
	inQuote := false
	inToken := false
	flush := func() {
		if inToken {
			argv = append(argv, cur.String())
			cur.Reset()
			inToken = false
		}
	}
	for _, r := range s {
		switch {
		case r == '"':
			inQuote = !inQuote
			inToken = true
		case (r == ' ' || r == '\t') && !inQuote:
			flush()
		default:
			cur.WriteRune(r)
			inToken = true
		}
	}
	flush()
	return argv
}

// psRow is one `ps` line: the PID, and the single free-form column that
// followed it, verbatim.
type psRow struct {
	pid  int
	rest string
}

// parsePsRows splits `ps -o pid=,<one column>` output into rows. The column
// is free-form and may contain spaces, so only the leading PID is parsed;
// everything after it is returned untouched for the caller to interpret.
func parsePsRows(raw []byte) []psRow {
	var out []psRow
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		sp := strings.IndexAny(line, " \t")
		if sp < 0 {
			continue
		}
		pid, err := strconv.Atoi(line[:sp])
		if err != nil {
			continue
		}
		rest := strings.TrimSpace(line[sp+1:])
		if rest == "" {
			continue
		}
		out = append(out, psRow{pid: pid, rest: rest})
	}
	return out
}

// parsePsOutput parses `ps -axww -o pid=,command=` output on its own, with
// no program-path column to merge against. mergePsReads is the two-read
// form; this is the one-read fallback and what the tests drive directly.
func parsePsOutput(raw []byte) []ProcInfo { return mergePsReads(raw, nil) }

// mergePsReads joins `ps -o pid=,command=` (argv, space-joined) with
// `ps -o pid=,comm=` (the executable path, alone on the line) BY PID.
//
// By PID, never by position: the two reads are two `ps` executions, so
// processes appear and vanish between them and the slices do not line up.
// A PID present in only one read is not an error — a process that exited
// between the reads simply has no program path and falls back to splitting
// argv[0] out of the command line, which is exactly the pre-#1303
// behaviour. commRaw nil or empty means "no second read", same fallback.
func mergePsReads(commandRaw, commRaw []byte) []ProcInfo {
	var programs map[int]string
	if rows := parsePsRows(commRaw); len(rows) > 0 {
		programs = make(map[int]string, len(rows))
		for _, r := range rows {
			programs[r.pid] = r.rest
		}
	}
	rows := parsePsRows(commandRaw)
	out := make([]ProcInfo, 0, len(rows))
	for _, r := range rows {
		program := programs[r.pid]
		argv := argvWithProgram(program, r.rest, strings.Fields)
		if len(argv) == 0 {
			continue
		}
		out = append(out, ProcInfo{PID: r.pid, Argv: argv, Program: program})
	}
	return out
}

// argvWithProgram rebuilds argv from a command-line STRING plus the program
// path the OS reported separately, using split for the argument tail.
//
// The join is decided, not guessed: the program path is accepted as argv[0]
// only when the command line actually begins with it (bare, or double-quoted
// as Windows writes it). Anything else — a process that rewrote its own
// argv[0], a PID reused between two reads — falls back to splitting the
// command line alone, which is what this did before waired-agent#1303.
//
// Only argv[0] is at stake. A space-bearing flag VALUE (`--model /Library/…`)
// still shatters into tokens, and that is deliberate: nothing reads
// RunnerFlags.ModelPath, while -np / -c are numeric and survive the split as
// their own tokens. Recovering values would need the real argv
// (KERN_PROCARGS2 on darwin), which this package's OS boundary deliberately
// does not reach for.
func argvWithProgram(program, commandLine string, split func(string) []string) []string {
	if program == "" {
		return split(commandLine)
	}
	quoted := `"` + program + `"`
	switch {
	case commandLine == program, commandLine == quoted:
		return []string{program}
	case strings.HasPrefix(commandLine, program+" "):
		return append([]string{program}, split(commandLine[len(program)+1:])...)
	case strings.HasPrefix(commandLine, quoted+" "):
		return append([]string{program}, split(commandLine[len(quoted)+1:])...)
	}
	return split(commandLine)
}

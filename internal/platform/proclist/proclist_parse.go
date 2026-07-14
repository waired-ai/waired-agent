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
		out = append(out, ProcInfo{PID: p.ProcessID, Argv: splitWindowsCmdline(p.CommandLine)})
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

// parsePsOutput parses `ps -axww -o pid=,command=` output: each line is
// leading space + PID + space + the full command with args. The command is
// whitespace-split into argv (Ollama blob paths carry no spaces, so this is
// sufficient to recover the -np/-c flags).
func parsePsOutput(raw []byte) []ProcInfo {
	var out []ProcInfo
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
		argv := strings.Fields(line[sp+1:])
		if len(argv) == 0 {
			continue
		}
		out = append(out, ProcInfo{PID: pid, Argv: argv})
	}
	return out
}

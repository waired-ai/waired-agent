// Package proclist enumerates the local process table with each process's
// full argv, so the agent can read a live model runner's actual flags —
// specifically the Ollama-spawned llama-server / `ollama runner` and its
// -np (num parallel) and -c (context) values, which /api/ps does not
// expose (waired#763).
//
// The OS boundary is intentionally thin: each _<os>.go file supplies only
// the I/O primitive `list()`, while all argv/command-line PARSING lives in
// this shared, untagged file so the parsers compile and unit-test on every
// platform (CI runs `go test` on Linux only). See proclist_linux.go /
// proclist_windows.go / proclist_darwin.go / proclist_others.go.
package proclist

import (
	"errors"
	"strconv"
	"strings"
)

// ErrUnsupported is returned by List on a GOOS with no runner-inspection
// implementation. Callers treat it like any other error: fall back to the
// intent value rather than failing.
var ErrUnsupported = errors.New("proclist: process enumeration not supported on this platform")

// ProcInfo is one process: its PID and full argv (argv[0] is the program).
type ProcInfo struct {
	PID  int
	Argv []string
}

// List returns the current process table. The per-OS list() does the I/O.
func List() ([]ProcInfo, error) { return list() }

// RunnerFlags is the subset of a llama.cpp / ollama-runner command line the
// tuning verification cares about. A zero field means the flag was absent.
type RunnerFlags struct {
	NumParallel int    // -np / --parallel
	ContextLen  int    // -c / --ctx-size  (llama.cpp: TOTAL across parallel slots)
	ModelPath   string // -m / --model
}

// IsRunnerProc reports whether argv looks like an Ollama model runner: a
// `llama-server[.exe]` process, or `ollama[.exe] runner …` (the in-process
// runner newer Ollama forks). basename is computed manually (both path
// separators) so the check is identical on every build OS and in tests.
func IsRunnerProc(argv []string) bool {
	if len(argv) == 0 {
		return false
	}
	base := strings.ToLower(baseName(argv[0]))
	base = strings.TrimSuffix(base, ".exe")
	switch base {
	case "llama-server":
		return true
	case "ollama":
		for _, a := range argv[1:] {
			if a == "runner" {
				return true
			}
		}
	}
	return false
}

// baseName returns the final path element, splitting on both `/` and `\` so
// a Windows-style argv[0] parses the same way when the test runs on Linux.
func baseName(p string) string {
	if i := strings.LastIndexAny(p, `/\`); i >= 0 {
		return p[i+1:]
	}
	return p
}

// ParseRunnerFlags extracts -np/-c/-m from a runner argv, tolerating both
// `-c N` and `-c=N` (and the long forms). Unknown flags (e.g. --spec-type
// draft-mtp) are skipped. Values that do not parse as ints are ignored.
func ParseRunnerFlags(argv []string) RunnerFlags {
	var f RunnerFlags
	for i := 0; i < len(argv); i++ {
		name, inlineVal, hasInline := splitFlag(argv[i])
		valueAt := func() (string, bool) {
			if hasInline {
				return inlineVal, true
			}
			if i+1 < len(argv) {
				i++
				return argv[i], true
			}
			return "", false
		}
		switch name {
		case "-np", "--parallel":
			if v, ok := valueAt(); ok {
				if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
					f.NumParallel = n
				}
			}
		case "-c", "--ctx-size":
			if v, ok := valueAt(); ok {
				if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
					f.ContextLen = n
				}
			}
		case "-m", "--model":
			if v, ok := valueAt(); ok {
				f.ModelPath = v
			}
		}
	}
	return f
}

// splitFlag splits a "--flag=value" token into ("--flag", "value", true);
// a bare "--flag" returns ("--flag", "", false).
func splitFlag(tok string) (name, val string, hasVal bool) {
	if !strings.HasPrefix(tok, "-") {
		return tok, "", false
	}
	if i := strings.IndexByte(tok, '='); i >= 0 {
		return tok[:i], tok[i+1:], true
	}
	return tok, "", false
}

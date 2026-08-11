package main

import (
	"fmt"
	"io"

	"github.com/waired-ai/waired-agent/internal/platform/logrotate"
)

// logFileOpener opens the agent's own rotating log file. It is a parameter
// rather than a direct call so the (GOOS, state dir) -> plan decision below
// can be table-tested on any host, with a fake that records the path and
// policy it was handed (CLAUDE.md §Test discipline).
type logFileOpener func(path string, p logrotate.Policy) (io.WriteCloser, error)

// openAgentLogFile opens the log file the agent writes its own records to,
// or returns (nil, nil) on an OS where the service manager already holds
// the stream — Linux's journal, macOS's launchd capture. Only Windows
// needs one: under the SCM stderr is closed and the Event Log takes Warn
// and above, so INFO and DEBUG have nowhere to land (#636).
//
// A failure to open is returned, not fatal. The daemon still runs and
// still logs to stderr; the caller reports the error once the logger
// exists. On a service install %ProgramData%\waired is locked to
// SYSTEM + Administrators, so this is where a misconfigured ACL would
// surface.
func openAgentLogFile(goos, stateDir string, open logFileOpener) (io.WriteCloser, string, error) {
	path := logrotate.AgentOwnedLogFile(goos, stateDir)
	if path == "" {
		return nil, "", nil
	}
	f, err := open(path, logrotate.DefaultPolicy())
	if err != nil {
		return nil, path, fmt.Errorf("open the agent log file %s: %w", path, err)
	}
	return f, path, nil
}

// openRotatingLogFile is the production logFileOpener. The explicit nil on
// the error path matters: returning logrotate.OpenFile's results straight
// through would hand back an io.WriteCloser holding a nil *logrotate.File,
// which is not == nil and would be written to.
func openRotatingLogFile(path string, p logrotate.Policy) (io.WriteCloser, error) {
	f, err := logrotate.OpenFile(path, p)
	if err != nil {
		return nil, err
	}
	return f, nil
}

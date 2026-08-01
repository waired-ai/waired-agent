//go:build windows

package logrotate

// defaultOps returns a zero ops on Windows, which makes Manage return
// without starting anything.
//
// Nothing to do: under the SCM the service's stderr is closed, so the
// agent's high-severity records go to the Application Event Log through
// internal/platform/logsink, and the Event Log is bounded by its own
// retention policy. There is no plain log file this process holds a
// descriptor to.
func defaultOps() ops { return ops{} }

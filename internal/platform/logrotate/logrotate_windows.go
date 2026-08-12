//go:build windows

package logrotate

// defaultOps returns a zero ops on Windows, which makes Manage return
// without starting anything.
//
// Nothing for this mechanism to do: it re-points a descriptor a service
// manager opened, and under the SCM the service's stderr is closed, so
// there is no such descriptor.
//
// The records still have somewhere to go, in two places neither of which
// is rotated from here. Warn and above reach the Application Event Log
// through internal/platform/logsink, bounded by the Event Log's own
// retention policy. Info and Debug reach the file AgentOwnedLogFile
// names: cmd/waired-agent opens it through OpenFile at startup and holds
// it for the life of the process, and File keeps it within the same
// Policy this package applies everywhere else (#687). That one is a
// handle the writer owns outright rather than a descriptor it borrowed,
// so it rotates itself from the inside.
func defaultOps() ops { return ops{} }

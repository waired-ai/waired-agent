//go:build windows

package appcontrol

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/windows"
)

// maxRecords caps one query. servicediag reads 20, which is right for "why did
// the service fail to start at boot" and wrong here: the run this package was
// written for produced 234 refusals of one file in 91 minutes, and a cap of 20
// would have reported a fraction of it and dated the window from the wrong end.
const maxRecords = 500

// sinceBoot is how far back to look. A refusal that stopped before the machine
// last started is history, not something to tell the user about now. The
// uptime counter rather than the log's own boot records: it cannot be filtered
// out by retention, and it needs no query.
func sinceBoot() time.Time {
	return time.Now().Add(-windows.DurationSinceBoot())
}

// Check reads Microsoft-Windows-CodeIntegrity/Operational for refusals of
// Waired's own programs since boot.
//
// The channel is readable without elevation — measured, unelevated, on two
// Windows 11 hosts (docs/knowledges/20260904/0300-…), which is what makes this
// usable from the tray, a per-user process.
//
// wevtutil rather than PowerShell, and by absolute path from the system
// directory, for the reasons servicediag's collector gives: it is on every
// edition, needs no execution policy, emits XML rather than formatted text,
// and the caller's PATH is not to be trusted.
func Check(ctx context.Context) Result {
	records, read := query(ctx, sinceBoot())
	if !read {
		// The log could not be read. That is NOT the same answer as "nothing
		// was refused", and reporting it as Clear would be the failure shape
		// this whole package exists to close: a silence that reads as good
		// news. NotApplicable makes callers say nothing instead.
		return Result{Status: NotApplicable}
	}
	return Explain("windows", records)
}

// query returns the records it read and whether the read happened at all. The
// second value is the difference between "Windows has refused nothing" and
// "this process could not look", and the caller must not conflate them.
func query(ctx context.Context, cutoff time.Time) ([]Record, bool) {
	exe, err := systemExe("wevtutil.exe")
	if err != nil {
		return nil, false
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	ids := []int{EventBlocked, EventEnforced, EventBlockDetail}
	clause := make([]string, 0, len(ids))
	for _, id := range ids {
		clause = append(clause, "EventID="+strconv.Itoa(id))
	}
	cmd := exec.CommandContext(ctx, exe, "qe",
		"Microsoft-Windows-CodeIntegrity/Operational",
		"/q:*[System[("+strings.Join(clause, " or ")+")]]",
		"/c:"+strconv.Itoa(maxRecords), "/rd:true", "/f:xml")
	out, err := cmd.Output()
	if err != nil {
		// A missing channel or an access denial. wevtutil exits 0 with empty
		// output when the query simply matches nothing, so this branch really
		// is "could not look" rather than "nothing there".
		return nil, false
	}
	return parseRecords(out, cutoff)
}

// systemExe resolves a Windows-supplied tool inside %SystemRoot%\System32.
func systemExe(name string) (string, error) {
	dir, err := windows.GetSystemDirectory()
	if err != nil {
		return "", err
	}
	p := filepath.Join(dir, name)
	if _, err := os.Stat(p); err != nil {
		return "", err
	}
	return p, nil
}

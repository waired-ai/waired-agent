//go:build windows

package servicediag

import (
	"context"
	"encoding/xml"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/windows"

	"github.com/waired-ai/waired-agent/internal/platform/service"
)

// Check reads the recent System-log history for the agent service.
//
// wevtutil, not PowerShell: it ships with every Windows edition, needs no
// execution-policy negotiation, and emits XML we can parse rather than
// formatted text we would have to scrape. Absolute path from the system
// directory, because a doctor run inherits the user's PATH.
//
// The queries deliberately do not use event 7036 (the generic
// entered-running/entered-stopped pair). On the rc7 host the System log for
// the day of the failure contained no 7036 records at all, so a decoder keyed
// on it would have reported "nothing happened" about the one boot that failed.
func Check(ctx context.Context) Result {
	running := serviceRunning()

	var events []Event
	// SCM records name the service in their message text, so filter by
	// provider + event ID and match the name afterwards.
	events = append(events, queryEvents(ctx, "System",
		`*[System[Provider[@Name='Service Control Manager'] and (`+
			eventIDClause(winSCMStartFailed, winSCMStartTimeout, winSCMStartHung,
				winSCMLoginFailed, winSCMTerminated, winSCMActionTaken)+`)]]`,
		service.ServiceName)...)
	// CodeIntegrity records name the blocked file path instead.
	events = append(events, queryEvents(ctx, "Microsoft-Windows-CodeIntegrity/Operational",
		`*[System[(`+eventIDClause(winCodeIntegrityBlocked, winCodeIntegrityAudit)+`)]]`,
		service.ServiceName+".exe")...)

	return Explain("windows", running, events)
}

// serviceRunning asks the SCM, with read-only rights so an unelevated doctor
// run can answer (the same access pattern as service.Installed).
func serviceRunning() bool {
	scm, err := windows.OpenSCManager(nil, nil, windows.SC_MANAGER_CONNECT)
	if err != nil {
		return false
	}
	defer windows.CloseServiceHandle(scm)
	name, err := windows.UTF16PtrFromString(service.ServiceName)
	if err != nil {
		return false
	}
	h, err := windows.OpenService(scm, name, windows.SERVICE_QUERY_STATUS)
	if err != nil {
		return false
	}
	defer windows.CloseServiceHandle(h)
	var st windows.SERVICE_STATUS
	if err := windows.QueryServiceStatus(h, &st); err != nil {
		return false
	}
	return st.CurrentState == windows.SERVICE_RUNNING
}

// eventLogXML is the subset of the Windows event schema we read.
type eventLogXML struct {
	Events []struct {
		System struct {
			Provider struct {
				Name string `xml:"Name,attr"`
			} `xml:"Provider"`
			EventID     int `xml:"EventID"`
			TimeCreated struct {
				SystemTime string `xml:"SystemTime,attr"`
			} `xml:"TimeCreated"`
		} `xml:"System"`
		Data []string `xml:"EventData>Data"`
	} `xml:"Event"`
}

// queryEvents runs one wevtutil query and keeps the records whose data
// mentions `mentions`. Newest first, capped: this is a diagnosis, not an
// audit, and the failing boot is the most recent one that matters.
func queryEvents(ctx context.Context, logName, query, mentions string) []Event {
	exe, err := systemExe("wevtutil.exe")
	if err != nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, exe, "qe", logName,
		"/q:"+query, "/c:20", "/rd:true", "/f:xml")
	out, err := cmd.Output()
	if err != nil {
		// A missing channel, an access denial, or simply no matching records:
		// all mean "no evidence", which Explain renders as saying nothing.
		return nil
	}
	// wevtutil emits a bare sequence of <Event> elements, which is not a
	// document. Wrap it so encoding/xml will parse it.
	var parsed eventLogXML
	if err := xml.Unmarshal([]byte("<Events>"+string(out)+"</Events>"), &parsed); err != nil {
		return nil
	}

	var events []Event
	for _, e := range parsed.Events {
		msg := strings.TrimSpace(strings.Join(e.Data, " "))
		if mentions != "" && !strings.Contains(strings.ToLower(msg), strings.ToLower(mentions)) {
			continue
		}
		src := e.System.Provider.Name
		if when := e.System.TimeCreated.SystemTime; when != "" {
			src = fmt.Sprintf("%s (%s)", src, when)
		}
		events = append(events, Event{Source: src, ID: e.System.EventID, Message: msg})
	}
	return events
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

// unused keeps strconv imported for the ID clause builder below on all
// toolchain versions.
var _ = strconv.Itoa

func eventIDClause(ids ...int) string {
	parts := make([]string, 0, len(ids))
	for _, id := range ids {
		parts = append(parts, "EventID="+strconv.Itoa(id))
	}
	return strings.Join(parts, " or ")
}

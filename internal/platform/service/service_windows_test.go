//go:build windows

package service

import (
	"slices"
	"testing"

	"golang.org/x/sys/windows"
)

// PRODUCT CONTRACT (#175). A presence check must succeed for a caller with
// no special privileges: `waired init` uses it to tell "the background
// service is registered but isn't answering" apart from "there is no agent
// on this host at all", and the first of those is exactly what a
// non-elevated user hits when the service failed to start.
//
// The previous implementation used mgr.Connect(), which opens the SCM with
// SC_MANAGER_ALL_ACCESS and so fails outright without Administrator —
// reporting every service, including a registered waired-agent, as absent.
//
// This pins the rights rather than the outcome on purpose: CI's Windows
// runner is already elevated, so an outcome-based test would pass with the
// old code too and prove nothing.
func TestInstalledAccessRightsAreReadOnly(t *testing.T) {
	if extra := installedSCMAccess &^ windows.SC_MANAGER_CONNECT; extra != 0 {
		t.Errorf("installedSCMAccess carries extra rights 0x%x beyond SC_MANAGER_CONNECT; "+
			"anything more needs Administrator and re-breaks the non-elevated presence check", extra)
	}
	if extra := installedServiceAccess &^ windows.SERVICE_QUERY_STATUS; extra != 0 {
		t.Errorf("installedServiceAccess carries extra rights 0x%x beyond SERVICE_QUERY_STATUS", extra)
	}
}

// The read path actually resolves a real service. At least one of these is
// registered on every Windows install (and on the GitHub-hosted runner);
// requiring only one keeps the test off any single service's presence.
func TestInstalledNamedFindsARegisteredService(t *testing.T) {
	candidates := []string{"EventLog", "Schedule", "Dnscache", "Winmgmt", "LanmanWorkstation"}
	if !slices.ContainsFunc(candidates, installedNamed) {
		t.Errorf("installedNamed found none of %v — the SCM read path is broken", candidates)
	}
}

func TestInstalledNamedRejectsAnAbsentService(t *testing.T) {
	if installedNamed("waired-agent-no-such-service-175") {
		t.Error("installedNamed reported an unregistered service as installed")
	}
}

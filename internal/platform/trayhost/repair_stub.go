//go:build !linux

package trayhost

import (
	"context"
	"errors"
	"io"
)

// The repair half is Linux-only for the same reason Check is: macOS and Windows
// draw tray icons natively, so there is no host extension to install or enable
// and nothing that could be missing. Check already returns NotApplicable here,
// which makes PlanRepair return RepairNone, so Install and Enable are
// unreachable through the normal flow — they exist to keep the package's API
// identical on all three OSes (CLAUDE.md §Cross-OS parity).

var errNotSupported = errors.New("trayhost: the AppIndicator host extension is a Linux-desktop concern only")

// GatherRepairFacts reports facts that always plan to RepairNone off Linux.
func GatherRepairFacts(r Result) RepairFacts {
	return RepairFacts{Status: r.Status, Desktop: r.Desktop}
}

// Plan always returns RepairNone off Linux.
func Plan(Result) RepairAction { return RepairNone }

// Install is never applicable off Linux.
func Install(context.Context, io.Writer) error { return errNotSupported }

// Enable is never applicable off Linux.
func Enable(context.Context) error { return errNotSupported }

// RepairPackage and RepairUUID have no meaning off Linux; they return empty so
// callers that print them render nothing rather than Linux-only advice.
func RepairPackage() string { return "" }
func RepairUUID() string    { return "" }

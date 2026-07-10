// Package detect inspects the user's shells and IDEs for an existing
// `waired claude` wrapper configuration, reporting (per location)
// whether the configuration is present and whether it points at the
// currently-running waired binary. Designed for the management API to
// surface to the system tray; pure functions, no writes.
//
// "stale" detection (configured but pointing at the wrong binary) is
// the silent-breakage class of bug we are guarding against — without
// it, a leftover entry from a prior install path would re-introduce
// the very failure the wrapper subcommand was built to prevent.
package detect

// Result describes one detection site (one rc file, one settings.json,
// one JetBrains options file).
type Result struct {
	Path         string `json:"path"`
	Flavor       string `json:"flavor,omitempty"`
	Configured   bool   `json:"configured"`
	Stale        bool   `json:"stale"`
	CurrentValue string `json:"current_value,omitempty"`
	// Note records why a parse failed or what edge case was hit, so the
	// tray can show "configured but unreadable" rather than silently
	// dropping the file from the report.
	Note string `json:"note,omitempty"`
}

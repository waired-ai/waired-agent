//go:build darwin

package main

// platformDefaultControlURL: macOS has no installer-written env-file
// convention yet, so the operator passes --control or sets
// $WAIRED_CONTROL_URL (already consulted as the flag default). Returns ""
// so the caller falls through. TODO: read a future
// ~/Library/Application Support/waired/agent.env if one is introduced.
func platformDefaultControlURL() string { return "" }

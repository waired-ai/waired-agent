// Package hostspeed holds the two install-time budgets that `waired init`
// and the daemon have to agree about, and that neither can see from the
// other.
//
// It exists because they disagreed. The #496 host-speed measurement runs
// in the daemon, in front of the bundled model's download; the wait for
// that download runs in the CLI. Both are `package main`, so the two
// numbers could not be compared even in principle, and they were not:
// waired-agent#579 bounded the measurement at 16 minutes while the wait it
// stands inside was 10. Measured on run 31316731884 — the daemon's pre-pull
// released at 14:28:49, the model was dispatched at 14:45:11, and the
// download itself took 21.9 seconds. It was never slow; init had stopped
// waiting six minutes before it started.
//
// Same lesson as proto/hostfit's package doc records for the 45 s
// threshold (waired-ai/waired#942): a figure with a second copy nobody can
// see is a figure nobody notices drifting. The difference here is that the
// two were never copies of each other — they were related numbers that no
// test could relate.
//
// Deliberately a leaf: stdlib only, no imports from either binary, so both
// can depend on it and the arithmetic between them is one test.
package hostspeed

import "time"

const (
	// ModelWait is how long `waired init` waits for the bundled model to
	// become ready before it says the download will finish in the
	// background (cmd/waired, benchPollDeadline). It is the window
	// everything on the install path has to fit inside.
	//
	// Not a deadline on the download: the transfer continues either way,
	// and init says so. It is a deadline on the TERMINAL — how long the
	// first run stays in front of an operator before handing off.
	ModelWait = 10 * time.Minute

	// InstallWindow is the most of ModelWait the host-speed measurement
	// may take while a download waits behind it (cmd/waired-agent,
	// hostSpeedInstallWindow).
	//
	// Half, and the half is sized against something real rather than
	// chosen: the reference host that hostfit.HostCutoffTurnBudgetSeconds
	// was derived from completes its whole three-sample measurement in
	// 4 s + 3 x 66.6 s = 204 s, and 4 s + 3 x 80.4 s = 245 s using the one
	// contended run from that same repeat. Both fit here with room. A host
	// slower than the anchor the budget was derived from hands the window
	// back to the download rather than spending the operator's install on
	// a verdict that arrives after the terminal has gone.
	//
	// The BACKGROUND measurement is not bounded by this. It runs on the
	// boot goroutine with nothing waiting behind it and keeps the full
	// budget, so the published median-of-three survives — the figure is
	// not the thing being cut short here, only the wait in front of a
	// download.
	defaultInstallWindow = 5 * time.Minute
)

// InstallWindow is defaultInstallWindow. A var rather than a const for the
// same reason benchPollDeadline and hostSpeedSettleWait are vars: the
// behaviour it governs is a WAIT, and a test that could not shrink it would
// have to spend the wait to observe it.
var InstallWindow = defaultInstallWindow

// SwapInstallWindowForTest sets InstallWindow and returns the restore.
//
// Machine-global, so a test using it must not run in parallel with one that
// reads the window — the same caveat every Swap*ForTest in this repo
// carries.
func SwapInstallWindowForTest(d time.Duration) func() {
	prev := InstallWindow
	InstallWindow = d
	return func() { InstallWindow = prev }
}

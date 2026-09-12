package main

import (
	"io"
	"time"
)

// The terminal half of waired-agent#1301.
//
// The owner's rc6 review asked that setup not be called complete until
// the benchmarks that follow the download have run. `waired init` waited
// for the install-time host-speed measurement (init_host_speed.go) and
// for the boot benchmark, but not for the mesh speed measurement that
// runs last — so on the reference host the completion box printed about
// four minutes before the work finished, with the GPU busy the whole
// time and peers being refused with `503 waired_inference_measuring`.
//
// This is also the implementation of the ruling recorded in
// waired-agent's
// docs/decisions/20260829/1740-speed-is-measured-at-fixed-depths.md —
// "3 分（オーナー裁定。init はこの計測を待つ）" — which until now had no
// code behind it: `grep measuring cmd/waired` found nothing.

var (
	// prefillWaitBudget bounds the wait. The measurement's own budget is
	// three minutes of engine time, but it only starts once it has the
	// engine, and on a host still finishing its boot benchmark that is a
	// few minutes later. Generous, and finite: a person watching a
	// terminal is owed an ending.
	prefillWaitBudget = 10 * time.Minute
	prefillWaitPoll   = 2 * time.Second
	// prefillNarrateEvery matches the host-speed wait's cadence, for the
	// reason recorded there: elapsed time is what distinguishes slow from
	// stuck, and this output is read through transcripts as often as by a
	// person.
	prefillNarrateEvery = 30 * time.Second
)

// prefillStageTerminal reports whether the daemon's
// prefill_measurement_stage means "no figure is still coming".
//
// The CLI keeps its own copy of these strings, the way init_host_speed.go
// keeps hostSpeedStageGaveUp's: the two binaries ship separately, so a
// CLI reading a stage an older daemon does not emit has to decide for
// itself, and a shared constant would not change that.
//
// An empty stage is terminal, and that is the compatibility case rather
// than an oversight: a daemon predating the field reports nothing.
func prefillStageTerminal(stage string) bool {
	switch stage {
	case "", "measured", "failed", "gave_up":
		return true
	}
	return false
}

// waitPrefillMeasurement blocks until the daemon says the mesh speed
// measurement is not going to produce anything more, or the budget runs
// out.
//
// Announced only once there is something to wait for. On a host that has
// already measured — a re-run of init, or a fast machine that finished
// during the model wait — the first poll is terminal and this prints
// nothing at all.
//
// A daemon predating prefill_measurement_stage reports nothing, which
// prefillStageTerminal reads as terminal: an older daemon must not cost
// a newer CLI its whole budget waiting for a signal that will never
// arrive.
func waitPrefillMeasurement(mgmtURL string, out io.Writer) {
	deadline := time.Now().Add(prefillWaitBudget)
	var narrated bool
	var startedAt, saidAt time.Time
	for {
		st, ok := fetchInferenceStatus(mgmtURL)
		if ok && prefillStageTerminal(st.PrefillMeasurementStage) {
			if narrated {
				writePromptf(out, "   done, %s\n", time.Since(startedAt).Round(time.Second))
			}
			return
		}
		if time.Now().After(deadline) {
			if narrated {
				// Said rather than dropped: the box below is about to
				// call this computer set up, and something on it is
				// still working.
				writePromptf(out, "   still going after %s; carrying on\n",
					time.Since(startedAt).Round(time.Second))
			}
			return
		}
		switch {
		case !narrated:
			writePromptf(out, "%s Timing this computer on the model it will serve. A few minutes...\n",
				emo("⏱", "*"))
			narrated, startedAt, saidAt = true, time.Now(), time.Now()
		case time.Since(saidAt) >= prefillNarrateEvery:
			writePromptf(out, "   still timing, %s so far\n",
				time.Since(startedAt).Round(time.Second))
			saidAt = time.Now()
		}
		time.Sleep(prefillWaitPoll)
	}
}

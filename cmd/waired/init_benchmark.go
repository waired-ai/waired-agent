package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/waired-ai/waired-agent/internal/hostspeed"
	"github.com/waired-ai/waired-agent/internal/management"
	"github.com/waired-ai/waired-agent/internal/management/ipcclient"
	notices "github.com/waired-ai/waired-agent/internal/notice"
	"github.com/waired-ai/waired-agent/proto/hostfit"
)

// benchPollDeadline bounds how long `waired init` waits for the model to
// finish downloading + the engine to come up before it gives up on the
// interactive-performance check. It bounds that wait only: once the
// measurement itself is running, init waits for it to end however long it
// takes, because setup is not complete until it has (waired-ai/waired#1382;
// decision 5 of docs/decisions/20260913/2245). Generous because a cold first pull of a
// multi-GB model over a slow link can take many minutes; the user can
// always re-run `waired runtimes benchmark` later. A var (not const) so
// tests can shrink it.
//
// The figure itself lives in internal/hostspeed because the daemon has to
// fit inside it and could not see it: the #496 measurement runs in front of
// the download this waits for, and it was bounded at 16 minutes against
// this 10 (waired-agent#579). Two `package main`s, no test that could
// compare them. Changing the number here now moves the daemon's share with
// it, and hostspeed's own test says whether the two still add up.
var benchPollDeadline = hostspeed.ModelWait

// benchPollInterval is the gap between status / benchmark polls. A var so
// tests can shrink it to stay fast.
var benchPollInterval = 3 * time.Second

// benchNoEngineGrace bounds how long we tolerate the transient `no_engine`
// state before treating it as terminal. On a fresh bundled install the
// engine binary is still being installed / the child still being brought
// up at the first polls, so /status briefly reports `no_engine` even though
// the engine is on its way (issue #489). We keep polling through that
// window; only if `no_engine` outlives the grace do we conclude the engine
// will never come up (e.g. the bundled runtime is genuinely missing) and
// skip. A cold engine bring-up (binary extract + StartupReadyTimeout) fits
// comfortably inside this; a var so tests can shrink it.
var benchNoEngineGrace = 3 * time.Minute

// benchNarrateEvery is how often the wait says how long the measurement has
// been running. Elapsed time is what tells slow from stuck, and this output
// is read through transcripts as often as by a person. A var so tests can
// shrink it.
var benchNarrateEvery = 30 * time.Second

// promptBenchmarkRecommendation is `waired runtimes benchmark`: it asks the
// daemon to measure the active model again (mode rerun — a person asking for
// a new number overwrites the stored one) and, when one request takes longer
// than the line and a lighter model fits, prompts the user to switch (issue
// #133; waired-ai/waired-agent#1341). It NEVER switches
// without confirmation; --non-interactive prints the recommendation but
// does not auto-accept.
//
// It is best-effort: any transport / not-configured / timeout condition
// prints an informational line (or nothing) and returns nil so it never
// blocks `waired init` from succeeding.
// sc is the line source for its prompts. `waired runtimes benchmark`
// hands it an owner on a terminal so the model-switch wait can offer the
// Enter escape without a second reader (#223); every other caller passes
// its own scanner.
func promptBenchmarkRecommendation(mgmtURL string, nonInteractive bool, out io.Writer, sc lineReader, tty bool) error {
	// The ran-and-failed signal is for the `waired init` summary box, not
	// for this caller: `waired runtimes benchmark` has already printed the
	// engine's refusal in full, and turning it into a non-nil error here
	// would make an informational path start failing.
	_, _, err := benchmarkWithScanner(mgmtURL, management.BenchmarkModeRerun, nonInteractive, out, sc, tty)
	return err
}

// benchmarkOutcome carries the just-measured request time up to the final
// success summary. The zero value means "no measurement" — the benchmark was
// skipped, the daemon was unreachable, or an older daemon reported no
// seconds.
type benchmarkOutcome struct {
	Measured bool
	// Speed is the daemon's measurement and verdict in seconds per request
	// (waired-ai/waired-agent#1341): the figure, the line it was judged
	// against, and whether it is over.
	//
	// Carried to the closing box because the box is where a run that KEPT
	// a model over the line reports itself, and it had no way to say so: on
	// the rc6 review's RTX 4070 Laptop, `--non-interactive
	// --inference-enabled=true` measured a model too slow for a coding
	// agent, printed "Non-interactive: keeping Qwen3.5 9B", and then closed
	// on "setup is complete" with "Claude routed through Waired"
	// (waired-agent#1300).
	//
	// Read off the response rather than from the arm that kept the model,
	// so every path agrees: the accept path re-measures after the switch
	// (remeasureAfterSwitch), so its response describes the model this
	// computer actually ends up serving.
	Speed management.SpeedMeasurement
	// ModelID is the model the figure was measured on, so the summary can
	// report it as one model's speed rather than as an unattributed
	// figure (waired-agent#1027). Empty against a daemon that predates
	// the field, and the summary then prints the figure alone.
	ModelID string
}

// OverLine reports the daemon's verdict that one request takes longer than
// the line.
func (o benchmarkOutcome) OverLine() bool { return o.Measured && overLine(o.Speed) }

// overLine is the verdict a response carries. Read from over_budget, and
// from the two figures beside it as well, so a response that sent the
// figures and not the flag is still judged the way the daemon judges.
func overLine(sm management.SpeedMeasurement) bool {
	if sm.OverBudget {
		return true
	}
	if sm.BudgetSeconds <= 0 {
		return false
	}
	return sm.TurnSeconds > sm.BudgetSeconds || (sm.TurnSeconds <= 0 && sm.TurnFloorSeconds > sm.BudgetSeconds)
}

// speedPhrase renders a measurement the one way every surface does
// (internal/notice.RequestSeconds): "228 s per request", or "190 s or more
// per request" for a measurement still running past the line.
func speedPhrase(sm management.SpeedMeasurement) string {
	return notices.RequestSeconds(sm.TurnSeconds, sm.TurnFloorSeconds) + " per request"
}

// speedTarget is the "(target: 190 s or less)" that goes beside it. A
// response that carried no line is judged against the one this build ships,
// which is the one the daemon it ships with reads.
func speedTarget(sm management.SpeedMeasurement) string {
	budget := sm.BudgetSeconds
	if budget <= 0 {
		budget = hostfit.ModelTurnBudgetSeconds
	}
	return notices.TargetClause(budget)
}

// outcomeFrom reduces a benchmark response to the summary-facing measurement.
func outcomeFrom(resp *management.BenchmarkRunResponse) benchmarkOutcome {
	if resp == nil || !resp.Judged() {
		return benchmarkOutcome{}
	}
	return benchmarkOutcome{
		Measured: true,
		Speed:    resp.SpeedMeasurement,
		ModelID:  resp.ModelID,
	}
}

// benchModelLabel names the measured model for a sentence: the id the
// daemon attached to the figure, else the active model, else "".
func benchModelLabel(mgmtURL, modelID string) string {
	if modelID == "" {
		modelID = activeModelForDisplay(mgmtURL)
	}
	if modelID == "" {
		return ""
	}
	return bundledModelLabelDefault(modelID)
}

// benchmarkWithScanner is the body of promptBenchmarkRecommendation,
// taking an already-constructed line source so a caller that already
// prompted on the same stdin (offerBenchmark's "run benchmark now?" gate,
// or the daemon path's stdin owner) can share one reader instead of
// layering two bufio readers over os.Stdin. tty
// selects the in-place progress rendering of the post-accept download wait.
// It returns the raw benchmark response (nil when no measurement could be
// obtained) so the caller can surface the throughput in the final success
// summary; the error is always nil today (every give-up path is
// best-effort) but kept for future use.
//
// mode is management.BenchmarkModeEnsure from `waired init` — a setup does
// not measure twice what the daemon has just measured on its own — and
// BenchmarkModeRerun from `waired runtimes benchmark`.
//
// The wait has no cap once the measurement runs (waired-ai/waired#1382).
// When it passes the line before it ends, the person is told then and
// offered the lighter model at that moment; staying keeps the measurement
// running and the flow waits for it to finish.
func benchmarkWithScanner(mgmtURL, mode string, nonInteractive bool, out io.Writer, sc lineReader, tty bool) (*management.BenchmarkRunResponse, bool, error) {
	// switchTo is the suggestion the person accepted while the measurement
	// was still running; declined is the target they turned down then, so
	// the same question is not asked twice in one run.
	var switchTo *management.BenchmarkRecommendation
	declined := ""
	offer := func(st management.BenchmarkStatusResponse) bool {
		rec := st.Recommendation
		if nonInteractive || rec == nil || rec.Dismissed || rec.ToModelID == "" {
			return false
		}
		to := bundledModelLabelDefault(rec.ToModelID)
		switch ynAsk(out, sc, fmt.Sprintf("Switch to %s? The measurement keeps running if you stay.", to), true) {
		case ynYes:
			switchTo = rec
			return true
		case ynNo:
			declined = rec.ToModelID
		}
		return false
	}
	resp, ok, ranAndFailed := waitForBenchmark(mgmtURL, mode, out, offer)
	if switchTo != nil {
		from := bundledModelLabelDefault(switchTo.FromModelID)
		to := bundledModelLabelDefault(switchTo.ToModelID)
		var after *management.BenchmarkRunResponse
		if switchAndWait(mgmtURL, switchTo.ToModelID, to, out, sc, tty) {
			after = remeasureAfterSwitch(mgmtURL, out)
			offerToRemoveRejected(mgmtURL, switchTo.FromModelID, from, nonInteractive, out, sc)
		}
		return after, false, nil
	}
	if !ok {
		// already explained inside waitForBenchmark
		return nil, ranAndFailed, nil
	}
	sm := resp.SpeedMeasurement

	if rec := resp.Recommendation; rec != nil && !rec.Dismissed {
		// Special case: the step-down lands on the lightest model we
		// offer. There's nothing lighter to fall back to after it, so
		// instead of the neutral "switch to a lighter model" flow, confirm
		// whether to keep local inference at all (drop to it) or turn it
		// off. Default No.
		if isLightestOfferedModel(rec.ToModelID) && declined != rec.ToModelID {
			return tinyBenchmarkDisableFlow(mgmtURL, nonInteractive, out, sc, tty, rec, resp)
		}

		// Over the line → lighter-model flow (issue #133).
		from := bundledModelLabelDefault(rec.FromModelID)
		to := bundledModelLabelDefault(rec.ToModelID)
		writePromptf(out, "\n%s Local inference is slow: %s takes %s %s.\n",
			emo("🐢", "!"), from, speedPhrase(sm), speedTarget(sm))

		if declined == rec.ToModelID {
			// Asked while the measurement ran, and answered No then. The
			// finished figure does not change the question, so the answer
			// stands and is recorded the way the prompt below records it.
			if err := dismissRecommendation(mgmtURL, rec.FromVariantID, rec.ToVariantID); err != nil {
				writePromptf(out, "Warning: couldn't record your choice: %v\n", err)
			} else {
				writePromptf(out, "Keeping %s. You can switch later from the Waired app or with `waired runtimes benchmark`.\n",
					from)
			}
			return resp, false, nil
		}
		writePromptf(out, "Waired recommends switching from %s to %s, which answers faster and fits in this computer's memory.\n",
			from, to)

		if nonInteractive {
			writePromptf(out, "Non-interactive: keeping %s. Run `waired runtimes benchmark` to switch interactively.\n",
				from)
			return resp, false, nil
		}

		// Default Yes: stepping down is cheap and the host is struggling.
		// An exhausted stdin is not that Yes — see noAnswerKeeps.
		answer := ynAsk(out, sc, fmt.Sprintf("Switch to %s?", to), true)
		if answer == ynNoAnswer {
			return noAnswerKeeps(out, from, resp)
		}
		if answer == ynNo {
			if err := dismissRecommendation(mgmtURL, rec.FromVariantID, rec.ToVariantID); err != nil {
				writePromptf(out, "Warning: couldn't record your choice: %v\n", err)
			} else {
				writePromptf(out, "Keeping %s. You can switch later from the Waired app or with `waired runtimes benchmark`.\n",
					from)
			}
			return resp, false, nil
		}
		if switchAndWait(mgmtURL, rec.ToModelID, to, out, sc, tty) {
			resp = remeasureAfterSwitch(mgmtURL, out)
			offerToRemoveRejected(mgmtURL, rec.FromModelID, from, nonInteractive, out, sc)
		}
		return resp, false, nil
	}

	// Over the line with no lighter model to propose. Reaching here
	// means the daemon judged the request too slow and LighterCandidate
	// found nothing — most often because this host is already serving
	// the smallest model Waired offers, which is the one case where
	// "switch to something lighter" has no answer (waired-agent#784).
	//
	// The check is isLightestOfferedModel, not the absence of a
	// recommendation: a proposal can also be missing because the engine
	// pick failed or the measurement describes a model that is no longer
	// active, and offering to turn local inference off for either of
	// those would be answering a question nobody asked.
	if overLine(sm) && resp.Recommendation == nil {
		if modelID := activeModelForDisplay(mgmtURL); modelID != "" && isLightestOfferedModel(modelID) {
			return noLighterModelFlow(mgmtURL, nonInteractive, out, sc, modelID, resp)
		}
	}

	// Inside the line: a 200 means the daemon ran a real generation —
	// this doubles as the end-to-end "local inference works" smoke test.
	label := benchModelLabel(mgmtURL, resp.ModelID)
	switch {
	case sm.Judged() && !overLine(sm):
		if label != "" {
			writePromptf(out, "%s Local inference works. %s takes %s on this computer.\n",
				emo("✅", "*"), label, speedPhrase(sm))
		} else {
			writePromptf(out, "%s Local inference works. %s on this computer.\n",
				emo("✅", "*"), speedPhrase(sm))
		}
	case sm.Judged():
		// Over the line, and the two arms above did not take it: the
		// daemon had no lighter model to propose and this host is not on
		// the smallest one, so something else — a failed engine pick, a
		// measurement describing a model that is no longer active —
		// stopped the proposal.
		//
		// Say the number and stop. "Local inference works" over a figure
		// the same run judged too slow is the claim waired-agent#784
		// reported from the badge; printing it here would be the same
		// untruth in the same run.
		if label != "" {
			writePromptf(out, "%s Local inference is slow here: %s takes %s %s.\n",
				emo("🐢", "!"), label, speedPhrase(sm), speedTarget(sm))
		} else {
			writePromptf(out, "%s Local inference is slow here: %s %s.\n",
				emo("🐢", "!"), speedPhrase(sm), speedTarget(sm))
		}
	default:
		// No seconds. On a current daemon a FAILED benchmark is a non-200
		// (handled in waitForBenchmark), so reaching here means an older
		// daemon that never reported the figure: we know a generation ran,
		// not how fast. Do NOT claim "works" — that wording is what turned
		// a dead engine into a green line (waired-agent#29), because a
		// failed run and a too-slow host both arrive here with no figure.
		writePrompt(out, emo("ℹ", "i")+" Benchmark ran, but this build of Waired doesn't report a "+
			"figure in seconds. Update Waired, then run `waired runtimes benchmark` to see it.")
	}

	return resp, false, nil
}

// noAnswerKeeps is what the switch prompts do when stdin ended before an
// answer arrived: nothing, said out loud.
//
// `waired init` cannot decide this from a TTY check the way
// `waired runtimes benchmark` does (cmd/waired/runtimes.go). A scripted
// install legitimately pipes its answers in — scripts/dev/installtest-windows.ps1
// runs `'0' | waired init …` with no --non-interactive — so forcing
// report-only mode off a terminal would take the keyboard away from
// every prompt that run means to answer. What is wrong is narrower:
// ynPrompt returned the DEFAULT once the pipe ran dry, so the two
// default-Yes prompts in this flow replaced the model and then offered
// to delete the weights it moved off, with nobody at the keyboard
// (waired-agent#754).
//
// The line is the one --non-interactive already prints. That flag is not
// set here, but the statement is true either way: this run had no one to
// ask, so it kept what the host had.
func noAnswerKeeps(out io.Writer, from string, resp *management.BenchmarkRunResponse) (*management.BenchmarkRunResponse, bool, error) {
	writePromptf(out, "Non-interactive: keeping %s. Run `waired runtimes benchmark` to switch interactively.\n",
		from)
	return resp, false, nil
}

// switchAndWait accepts the recommendation and, when the target model still
// needs a download, foreground-waits for it with progress — the machine
// should be usable when the flow returns (waired#774). A pending Enter
// backgrounds the wait; the agent owns the pull either way.
// It reports whether the new model is the one this host serves now, so a
// caller can measure it: false means the switch failed, or the download
// is still running in the background, and in both cases the next thing
// the engine answers with is the OLD model.
func switchAndWait(mgmtURL, modelID, label string, out io.Writer, sc lineReader, tty bool) bool {
	pmr, err := acceptRecommendation(mgmtURL, modelID)
	if err != nil {
		writePromptf(out, "Warning: couldn't switch the model: %v\n", err)
		return false
	}
	if !pmr.Downloading {
		writePromptf(out, "Switching to %s (already downloaded).\n", label)
		return true
	}
	// The Enter escape is a terminal gesture: it exists only when this run
	// owns stdin (init_stdin.go). Off a terminal the wait simply runs to
	// completion, which is what a scripted stdin did before too — EOF
	// never backgrounded anything.
	owner, _ := sc.(*stdinReader)
	if owner != nil {
		writePromptf(out, "Switching to %s. Downloading it now. Press Enter anytime to continue in the background.\n", label)
	} else {
		writePromptf(out, "Switching to %s. Downloading it now.\n", label)
	}
	return waitForModelSwitch(mgmtURL, modelID, out, tty, newBackgroundWatch(owner))
}

// offerToRemoveRejected offers to delete the weights of the model the
// step-down moved away from.
//
// The wizard had just judged that model wrong for this host and then
// left several gigabytes of it on disk with nothing pointing at it: on
// the reported 16 GB machine, 12 GB of models against 6.4 GB the host
// actually meant to keep (waired-agent#648). Nothing was ever going to
// load it again — the same run had replaced it.
//
// It is offered, not done. The bytes are re-downloadable but not free,
// and an operator who expects to move back up after adding memory has a
// real reason to keep them; that is a decision the person in front of
// the machine owns, not the wizard. Default No (owner decision
// 2026-09-13, decision 8 of docs/decisions/20260913/2245): switching back
// later reuses the stored measurement (decision 7), and it should not have
// to download the weights again either.
//
// Non-interactive keeps the weights and says so, with the command that
// removes them. Deleting gigabytes on nobody's authority is the one
// answer an unattended run must not give.
//
// A failure is reported and nothing else happens. The install is
// finished and correct either way — the model is a leftover, not a
// fault — so this never fails the flow.
func offerToRemoveRejected(mgmtURL, modelID, label string, nonInteractive bool, out io.Writer, sc lineReader) {
	if modelID == "" {
		return
	}
	// Never offer to delete the model this host is SERVING. The premise
	// of the question — "Waired is not using it any more" — is what makes
	// the offer safe at all, and waired-agent#754 produced a step-down whose
	// two sides were the same model, which would have walked an operator
	// through deleting the weights under the engine (DeleteModel drops
	// the weights, clears state.Active, and clears the preference).
	//
	// The picker fix means from == to can no longer be reached from here.
	// This is the invariant stated where it can be checked, so a future
	// path that reaches it does not have to rediscover the consequence.
	//
	// An active model that will not resolve leaves the offer standing, on
	// purpose: reading "" as a match would silently retire #648's cleanup
	// on every host whose status call fails, which is a far more common
	// state than the one guarded against.
	if active := canonicalBundledModelID(activeModelForDisplay(mgmtURL)); active != "" &&
		active == canonicalBundledModelID(modelID) {
		return
	}
	if nonInteractive {
		writePromptf(out, "Keeping %s. Remove it with `waired models rm %s`.\n", label, modelID)
		return
	}
	answer := ynAsk(out, sc, fmt.Sprintf("Remove %s? Waired isn't using it any more.", label), false)
	if answer == ynNoAnswer {
		// Same line the unattended arm above prints, for the same reason:
		// deleting gigabytes on nobody's authority is the one answer this
		// must not give (waired-agent#754).
		writePromptf(out, "Keeping %s. Remove it with `waired models rm %s`.\n", label, modelID)
		return
	}
	if answer == ynNo {
		writePromptf(out, "Keeping %s. Remove it later with `waired models rm %s`.\n", label, modelID)
		return
	}
	if _, err := httpDelete(mgmtURL + "/waired/v1/models/" + modelID); err != nil {
		writePromptf(out, "Warning: couldn't remove %s (%v). Remove it later with `waired models rm %s`.\n",
			label, err, modelID)
		return
	}
	writePromptf(out, "Removed %s.\n", label)
}

// remeasureAfterSwitch measures the model that is serving after a
// step-down, and returns its response — or nil when no measurement could
// be taken.
//
// It exists because the summary reported the rate that JUSTIFIED the
// switch as if it described the model doing the work: `Model 26 tok/s`
// under a host that had just moved off the model measured at 26 tok/s
// (waired-agent#648). One response was taken before the recommendation
// and reused afterwards.
//
// nil is a deliberate answer, and callers REPLACE the pre-switch
// response with it rather than falling back. Keeping the old one would
// re-publish the very number this flow exists to move away from, now
// under a model that is not the one it was measured on. A summary with
// no rate row says less and nothing false, and waitForBenchmark has
// already explained whatever went wrong.
//
// It asks in mode ensure: a model this host has measured before is answered
// from the stored figure (decision 7 of docs/decisions/20260913/2245), and
// one it has not is measured. The daemon answers 425 while the engine is
// still bouncing around the new weights, which waitForBenchmark already
// polls through.
//
// Any recommendation the second run carries is deliberately ignored. The
// lighter model can itself measure over the line, and acting on that here
// would step down again inside a flow the operator answered once.
// `waired runtimes benchmark` is where that conversation belongs.
func remeasureAfterSwitch(mgmtURL string, out io.Writer) *management.BenchmarkRunResponse {
	writePrompt(out, "Measuring the new model...")
	resp, ok, _ := waitForBenchmark(mgmtURL, management.BenchmarkModeEnsure, out, nil)
	if !ok || resp == nil || !resp.Judged() {
		return nil
	}
	// The second run's own verdict decides the wording. Claiming "works"
	// over a figure this very run judged over the line was the same
	// untruth waired-agent#784 reported from the badge — and it was
	// reachable: a step-down onto a model that is itself too slow is
	// exactly what the chain exists to walk.
	//
	// The recommendation the second run carries is still ignored (see
	// above); what is NOT ignored any more is the measurement. Saying
	// where the host stands leaves the operator with something to act
	// on, and the catalog badge has already moved to the next rung by
	// the time this prints.
	sm := resp.SpeedMeasurement
	label := benchModelLabel(mgmtURL, resp.ModelID)
	if overLine(sm) {
		if label != "" {
			writePromptf(out, "%s %s takes %s here %s.\n",
				emo("🐢", "!"), label, speedPhrase(sm), speedTarget(sm))
		} else {
			writePromptf(out, "%s %s here %s.\n",
				emo("🐢", "!"), speedPhrase(sm), speedTarget(sm))
		}
		writePrompt(out, "   Run `waired runtimes benchmark` to look for a faster model again.")
		return resp
	}
	if label != "" {
		writePromptf(out, "%s Local inference works. %s takes %s on this computer.\n",
			emo("✅", "*"), label, speedPhrase(sm))
	} else {
		writePromptf(out, "%s Local inference works. %s on this computer.\n",
			emo("✅", "*"), speedPhrase(sm))
	}
	return resp
}

// tinyBenchmarkDisableFlow is the benchmark-time counterpart of the install
// spec-check dialog: one request with the active model takes longer than the
// line and the ONLY lighter step-down is the bottom of the ladder — nothing Waired
// offers is ranked below it (isLightestOfferedModel, init_modelselect.go).
// Rather than the neutral "switch to a lighter model" flow, it confirms whether
// to keep local inference by dropping to that last model, or turn it off.
// Default No → disable local inference; the node keeps working as a
// gateway/relay.
//
// It is an ordering, not a floor: #522 (owner decision 2026-08-08) removed the
// install quality floor this branch used to test, because a tier threshold
// could not say what it was being asked to say. This doc and the prompt below
// were left behind saying it anyway (waired-agent#834).
func tinyBenchmarkDisableFlow(
	mgmtURL string, nonInteractive bool, out io.Writer, sc lineReader, tty bool,
	rec *management.BenchmarkRecommendation, resp *management.BenchmarkRunResponse,
) (*management.BenchmarkRunResponse, bool, error) {
	from := bundledModelLabelDefault(rec.FromModelID)
	label := bundledModelLabelDefault(rec.ToModelID)
	writePromptf(out, "\n%s Local inference is slow here: %s takes %s %s.\n",
		emo("⚠", "!"), from, speedPhrase(resp.SpeedMeasurement), speedTarget(resp.SpeedMeasurement))
	// "very low quality" was the old wording. It said the wrong thing
	// twice: the install floor is not a measurement of quality, and #537
	// gives `small` a meaning that reaches models this flow would happily
	// recommend — so two lines of the product would have used one word
	// for two different lines.
	//
	// Its replacement ("sits below the bar Waired uses for coding — not
	// recommended on any computer") kept asserting the floor itself, which
	// #522 abolished; the branch is selected by an ordering. So the line
	// now says only what the gate actually tested (waired-agent#834).
	writePromptf(out, "   %s is the smallest model Waired offers, so there's nothing lighter to switch to after it.\n", label)

	if nonInteractive {
		writePromptf(out, "Non-interactive: keeping %s. Run `waired runtimes benchmark` to revisit.\n", from)
		return resp, false, nil
	}

	// Two-line question so the default and the "No disables it" clarifier read
	// as one prompt; ynPrompt appends the [y/N] (default: No) hint.
	q := "Drop to that model and keep local inference?\n" +
		"  No turns local inference off. This computer still routes requests to your other computers."
	answer := ynAsk(out, sc, q, false)
	if answer == ynNoAnswer {
		// Turning local inference off is a decision, not a fallback. The
		// default No is for a person who read the question; an exhausted
		// stdin means nobody did, and this arm used to disable inference
		// on a host nobody was watching (waired-agent#754).
		writePromptf(out, "Non-interactive: keeping %s. Run `waired runtimes benchmark` to revisit.\n", from)
		return resp, false, nil
	}
	if answer == ynYes {
		if switchAndWait(mgmtURL, rec.ToModelID, label, out, sc, tty) {
			resp = remeasureAfterSwitch(mgmtURL, out)
			// The same leftover as the ordinary step-down: this host was
			// measured too slow for the model it just moved off.
			offerToRemoveRejected(mgmtURL, rec.FromModelID, from, nonInteractive, out, sc)
		}
		return resp, false, nil
	}
	if err := disableLocalInference(mgmtURL); err != nil {
		writePromptf(out, "Warning: couldn't turn local inference off: %v\n", err)
	} else {
		writePrompt(out, "Local inference is off. This computer still routes requests to your other computers.")
	}
	return resp, false, nil
}

// noLighterModelFlow is what happens when the host is ALREADY on the
// bottom of the ladder and measured over the line (waired-agent#784).
//
// tinyBenchmarkDisableFlow next door handles the rung above this one:
// the step-down's target is the smallest model, so there is still a move
// to offer. Here the smallest model is what is running, so the only
// question left is whether to keep local inference at all. That is the
// owner's rule for this case — a machine that cannot run the lightest
// model Waired offers is under-specified, and whether to run anyway is
// the operator's call, not the wizard's.
//
// Default No, matching its sibling: this host has now measured the
// bottom of the ladder and come up short. An exhausted stdin is NOT
// that No — turning local inference off on a machine nobody was
// watching is waired-agent#754.
func noLighterModelFlow(
	mgmtURL string, nonInteractive bool, out io.Writer, sc lineReader,
	activeModelID string, resp *management.BenchmarkRunResponse,
) (*management.BenchmarkRunResponse, bool, error) {
	label := bundledModelLabelDefault(activeModelID)
	writePromptf(out, "\n%s Local inference is slow here: %s takes %s %s.\n",
		emo("⚠", "!"), label, speedPhrase(resp.SpeedMeasurement), speedTarget(resp.SpeedMeasurement))
	writePromptf(out, "   %s is the smallest model Waired offers, so there's nothing lighter to switch to.\n", label)

	if nonInteractive {
		writePromptf(out, "Non-interactive: keeping %s. Run `waired runtimes benchmark` to revisit.\n", label)
		return resp, false, nil
	}

	q := "Keep local inference on?\n" +
		"  No turns it off. This computer still routes requests to your other computers."
	switch ynAsk(out, sc, q, false) {
	case ynNoAnswer:
		writePromptf(out, "Non-interactive: keeping %s. Run `waired runtimes benchmark` to revisit.\n", label)
	case ynYes:
		writePromptf(out, "Keeping %s. You can turn local inference off later from the Waired app.\n", label)
	default:
		if err := disableLocalInference(mgmtURL); err != nil {
			writePromptf(out, "Warning: couldn't turn local inference off: %v\n", err)
		} else {
			writePrompt(out, "Local inference is off. This computer still routes requests to your other computers.")
		}
	}
	return resp, false, nil
}

// disableLocalInference POSTs the management soft-disable, which persists the
// desired-inference toggle so it survives daemon restarts.
func disableLocalInference(mgmtURL string) error {
	_, err := httpPost(mgmtURL+"/waired/v1/inference/disable", nil)
	return err
}

// waitForBenchmark polls the daemon until the engine + active model are
// ready, then runs the benchmark and returns the full response (the
// measurement plus any lighter-model suggestion). ok=false means "could not
// obtain a result" (daemon too old, model never readied within the deadline,
// terminal pull failure) — the caller should treat that as a non-error skip.
//
// ranAndFailed separates the ONE outcome inside ok=false that is not a
// skip: the benchmark reached the engine and the engine could not
// complete a generation (#29). Every other give-up here is a legitimate
// skip — a routing-only node, an external endpoint, a daemon too old —
// and #203 is explicit that those must not read as a fault. Without the
// distinction they were the same zero value, which is how `waired init`
// came to print "everything completed successfully!" one line after
// reporting HTTP 500 (waired-ai/waired-agent#552).
//
// While the measurement runs the wait says so, and how long it has been
// going; there is no deadline on that part (waired-ai/waired#1382). When
// it passes the line, the wait says that once, then calls offer (nil:
// nothing to offer) with the daemon's status. offer returning true means
// the person chose to leave the measurement — the wait abandons its
// request and returns ok=false, and the caller carries out the choice.
func waitForBenchmark(mgmtURL, mode string, out io.Writer, offer func(management.BenchmarkStatusResponse) bool) (resp *management.BenchmarkRunResponse, ok, ranAndFailed bool) {
	deadline := time.Now().Add(benchPollDeadline)
	// announcedWait is the lead of the wait line last printed, not a bool:
	// what init is waiting on can change mid-wait (the download finishes and
	// the engine starts loading), and the operator should be told when it
	// does — but only then.
	announcedWait := ""
	announcedEngine := false
	// noEngineDeadline is armed lazily on the first `no_engine` observation
	// and disarmed once any other state is seen, so a transient startup
	// `no_engine` is waited out but a host whose engine never comes up still
	// gives up after the grace rather than spinning to the full deadline.
	var noEngineDeadline time.Time
	engineSeen := false
	narr := &benchNarration{mgmtURL: mgmtURL, out: out, offer: offer}
	for {
		// Try the benchmark; the handler returns 425 until the engine and
		// model are both ready.
		status, body, err, abandoned := narr.post(benchmarkURL(mgmtURL, mode))
		if abandoned {
			return nil, false, false
		}
		if narr.measured {
			// A measurement ran on this request and did not end in a
			// result (a switch made elsewhere cancelled it). The engine
			// wait starts over from here, not from before the measurement.
			deadline = time.Now().Add(benchPollDeadline)
			narr.measured = false
		}
		switch {
		case err != nil:
			// Transport error — the daemon isn't reachable (not started
			// yet, or restarting). Tell the user how to act instead of
			// returning silently (the `waired runtimes benchmark` complaint).
			writePromptf(out, "Couldn't reach the background service at %s (%v).\nStart it, then run `waired runtimes benchmark`.\n", mgmtURL, err)
			return nil, false, false
		case status == http.StatusNotFound:
			// Older daemon without the benchmark endpoint.
			writePrompt(out, "This build of Waired doesn't support benchmarking yet. Skipping.")
			return nil, false, false
		case status == http.StatusOK:
			var r management.BenchmarkRunResponse
			if jErr := json.Unmarshal(body, &r); jErr != nil {
				writePromptf(out, "Benchmark returned an unreadable response (%v). Skipping.\n", jErr)
				return nil, false, false
			}
			return &r, true, false
		case status == http.StatusTooEarly:
			// Engine / model not ready yet. Consult /status to distinguish
			// "still loading" from a terminal failure so we don't spin for
			// the full deadline on a host that will never come up.
			state, failureReason := inferenceSubsystemState(mgmtURL)
			switch state {
			case "pull_failed":
				// The reason rides along (waired-agent#328). This refusal
				// used to be the whole of what a failed pull got told —
				// one fixed sentence, on the command the pull-failure hint
				// itself recommended.
				writePromptf(out, "Model download failed.%s Skipping the benchmark.\n",
					reasonSuffix(failureReason))
				return nil, false, false
			case "engine_failed":
				// waired-agent#1026. Terminal for this wait: the daemon
				// has stopped restarting the engine, so no model will
				// become ready and the poll would otherwise sit here for
				// the whole deadline — which is exactly what a host whose
				// vLLM could not bind its port did, with the benchmark
				// "taking longer than expected" as the only symptom.
				//
				// Worded like the pull_failed arm above rather than the
				// waiting arms below, because it is the same kind of
				// event: something the daemon already decided, with a
				// reason it already recorded.
				writePromptf(out, "The inference engine couldn't start.%s Skipping the benchmark.\n",
					reasonSuffix(failureReason))
				return nil, false, false
			case "disabled", "stopped":
				// Terminal, the same way waitForBundledModel already treats
				// them (init_pull.go): a subsystem that is off or parked will
				// never report a ready model, so waiting is waiting for
				// something nobody has asked to happen.
				//
				// This used to fall into the default arm below, which reads
				// any unrecognised state as "engine is up, download in
				// flight" — so `waired init --inference-enabled=false` sat on
				// "Waiting for the model to finish downloading…" for the full
				// ten-minute deadline and then reported it had given up, on a
				// host with no model and no intention of getting one. Found
				// by #175's installtest migration, which made this the path
				// CI takes: ten minutes per leg, three legs, every PR.
				writePrompt(out, "Local inference is off on this computer. Skipping the benchmark.")
				return nil, false, false
			case "no_engine":
				// On a fresh bundled install the engine is still being
				// brought up at the first polls, so `no_engine` is transient
				// (issue #489). Wait it out within a grace window rather than
				// skipping immediately; only conclude the engine will never
				// come up once the grace elapses with no engine ever seen.
				if !engineSeen {
					if noEngineDeadline.IsZero() {
						noEngineDeadline = time.Now().Add(benchNoEngineGrace)
					}
					if time.Now().After(noEngineDeadline) {
						writePrompt(out, "No inference engine is available. Skipping the benchmark.")
						return nil, false, false
					}
					if !announcedEngine {
						writePrompt(out, "Waiting for the inference engine to start before benchmarking... "+
							dim("(this can take a minute)"))
						announcedEngine = true
					}
				}
			case "":
				// /status was unreachable this tick — don't conclude the
				// engine is up (that would disarm the no_engine grace); just
				// keep polling and let the next tick re-read the state.
			default:
				// Engine is up (some non-no_engine state): disarm the
				// no_engine grace so a later blip can't cut the wait short,
				// and say which of the two remaining waits this is.
				engineSeen = true
				lead, hint := benchWaitLineFor(state)
				if lead != announcedWait {
					writePrompt(out, lead+" "+dim(hint))
					announcedWait = lead
				}
			}
		case status == http.StatusServiceUnavailable:
			// The benchmark ran and did not complete (waired-agent#29). The
			// engine is the thing to look at, so say so and point at the
			// tools that show why — never a success line.
			if msg := parseMgmtError(status, body).Message; msg != "" {
				writePromptf(out, "%s Local inference couldn't complete a test request: %s\n",
					emo("⚠", "!"), msg)
			} else {
				writePromptf(out, "%s Local inference couldn't complete a test request.\n", emo("⚠", "!"))
			}
			writePrompt(out, "  Check `waired status`, then `waired doctor`, for the engine's own reason.")
			return nil, false, true
		default:
			// Unexpected status — surface it (don't block init) instead of
			// exiting silently.
			writePromptf(out, "Benchmark unavailable (HTTP %d). Skipping.\n", status)
			return nil, false, false
		}

		if time.Now().After(deadline) {
			writePrompt(out, "Model not ready in time. Run `waired runtimes benchmark` later to check performance.")
			return nil, false, false
		}
		time.Sleep(benchPollInterval)
	}
}

// benchWaitLineFor is what init says it is waiting on while the daemon
// answers 425, derived from the subsystem state /status reported. lead is
// the sentence; hint is the parenthetical the caller dims.
//
// Two waits reach here and they are not the same thing. `awaiting_model`
// is the download. Everything else that gets this far is the engine
// bringing up a model that is ALREADY on disk — the states are `loading`
// and `starting`, plus `initializing` / `ready` / `degraded`, which are
// what a host in the readiness race reports for the moment between the
// model landing and the engine serving it (#576). `pull_failed`,
// `disabled`, `stopped` and `no_engine` never arrive here; they have their
// own arms above.
//
// Before this, every state that was not named printed the download line,
// so a host whose model was already ready was told to wait for a download
// that had finished — observed verbatim in a routing-sentinel transcript
// one line after `[ok] granite4-350m ready`. Record of today's behaviour,
// not a contract: the split follows the states, and a new state joins the
// engine side by default.
func benchWaitLineFor(state string) (lead, hint string) {
	if state == "awaiting_model" {
		return "Waiting for the model to finish downloading before benchmarking...", "(this can take a few minutes)"
	}
	return "Waiting for the inference engine to load the model before benchmarking...", "(this can take a minute)"
}

// inferenceSubsystemState GETs /inference/status and returns the
// subsystem_state plus the reason the daemon recorded for it. Both are ""
// on any error.
//
// The reason is chosen by the state, because the two terminal states keep
// it in different places: a failed download records it against the model
// (waired-agent#328), and a failed engine records it against the runtime
// (waired-agent#1026). Answering with the wrong one is worse than
// answering with nothing — it would put a download error in front of
// someone whose engine could not bind its port.
//
// One fetch rather than two: the caller decides what to say from the
// state and then has to say WHY, and a second round trip could answer
// from a different tick.
func inferenceSubsystemState(mgmtURL string) (state, failureReason string) {
	st, ok := fetchInferenceStatus(mgmtURL)
	if !ok {
		return "", ""
	}
	if st.SubsystemState == "engine_failed" {
		return st.SubsystemState, engineFailureDetail(st)
	}
	return st.SubsystemState, waitFailureReason(st, "")
}

// activeModelForDisplay resolves the just-benchmarked (active) model from
// /inference/status for the no-recommendation "works" line — the benchmark
// response itself doesn't name the model (waired#773). Empty on any error
// (old daemon, unreachable); callers fall back to model-less wording.
//
// It returned the variant id too, for the quality figure the label used to
// carry. #537 removed the figure and nothing else ever read the variant,
// so it is gone rather than left as a return value with no reader.
func activeModelForDisplay(mgmtURL string) string {
	st, ok := fetchInferenceStatus(mgmtURL)
	if !ok || st.Active == nil {
		return ""
	}
	return st.Active.ModelID
}

// acceptRecommendation POSTs the switch and returns the daemon's response —
// Downloading tells the caller whether a foreground download wait is worth
// starting (waired#774). A response an old daemon can't marshal decodes to
// the zero value (Downloading=false), which degrades to the pre-#774
// fire-and-forget behavior.
func acceptRecommendation(mgmtURL, modelID string) (management.PreferredModelResponse, error) {
	body, _ := json.Marshal(management.PreferredModelRequest{ModelID: modelID})
	respBody, err := httpPost(mgmtURL+"/waired/v1/inference/preferred-model", body)
	if err != nil {
		return management.PreferredModelResponse{}, err
	}
	var pmr management.PreferredModelResponse
	_ = json.Unmarshal(respBody, &pmr)
	return pmr, nil
}

func dismissRecommendation(mgmtURL, fromVariantID, toVariantID string) error {
	body, _ := json.Marshal(management.RecommendationDismissRequest{
		FromVariantID: fromVariantID,
		ToVariantID:   toVariantID,
	})
	_, err := httpPost(mgmtURL+"/waired/v1/inference/recommendation/dismiss", body)
	return err
}

// benchmarkURL is the benchmark endpoint with its mode.
func benchmarkURL(mgmtURL, mode string) string {
	u := mgmtURL + "/waired/v1/inference/benchmark"
	if mode == "" {
		return u
	}
	return u + "?mode=" + url.QueryEscape(mode)
}

// benchNarration is what the benchmark wait says while the daemon measures:
// that it started, how long it has been going, and — once — that it is past
// the line.
type benchNarration struct {
	mgmtURL string
	out     io.Writer
	offer   func(management.BenchmarkStatusResponse) bool

	// measured is true once a measurement has been seen running on the
	// request in flight; startedAt / saidAt time the progress lines.
	measured          bool
	announced         bool
	overSaid          bool
	startedAt, saidAt time.Time
}

// post sends the benchmark request and, while it is in flight, reads the
// daemon's benchmark status every benchPollInterval to narrate the
// measurement. abandoned reports that offer chose to leave the measurement;
// the request is cancelled before post returns.
//
// The request carries no client timeout. The daemon's measurement has none
// either once it runs (decision 4 of docs/decisions/20260913/2245), and a
// CLI budget shorter than the daemon's destroys the answer rather than
// making it late (see mgmtWriteRoute's note on budgets).
func (n *benchNarration) post(target string) (status int, body []byte, err error, abandoned bool) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type result struct {
		status int
		body   []byte
		err    error
	}
	done := make(chan result, 1)
	go func() {
		s, b, e := benchPost(ctx, target, nil)
		done <- result{s, b, e}
	}()
	tick := time.NewTicker(benchPollInterval)
	defer tick.Stop()
	for {
		select {
		case r := <-done:
			return r.status, r.body, r.err, false
		case <-tick.C:
			st, ok := fetchBenchmarkStatus(n.mgmtURL)
			if !ok || st.State != management.BenchmarkStateRunning {
				continue
			}
			if n.observe(st) {
				return 0, nil, nil, true
			}
		}
	}
}

// observe narrates one running status, and reports whether offer chose to
// leave the measurement.
func (n *benchNarration) observe(st management.BenchmarkStatusResponse) bool {
	now := time.Now()
	n.measured = true
	if !n.announced {
		writePromptf(n.out, "%s Timing this computer on the model it will serve. A few minutes...\n",
			emo("⏱", "*"))
		n.announced, n.startedAt, n.saidAt = true, now, now
	}
	sm := st.SpeedMeasurement
	if sm.OverBudget && !n.overSaid {
		n.overSaid = true
		label := benchModelLabel(n.mgmtURL, st.ModelID)
		if label != "" {
			writePromptf(n.out, "\n%s %s takes %s %s.\n", emo("🐢", "!"), label, speedPhrase(sm), speedTarget(sm))
		} else {
			writePromptf(n.out, "\n%s This computer takes %s %s.\n", emo("🐢", "!"), speedPhrase(sm), speedTarget(sm))
		}
		if n.offer != nil && n.offer(st) {
			return true
		}
		writePrompt(n.out, "   Waiting for the measurement to finish.")
		n.saidAt = time.Now()
		return false
	}
	if now.Sub(n.saidAt) >= benchNarrateEvery {
		elapsed := now.Sub(n.startedAt)
		if sm.ElapsedSeconds > 0 {
			elapsed = time.Duration(sm.ElapsedSeconds * float64(time.Second))
		}
		writePromptf(n.out, "   still measuring, %s so far %s\n", elapsed.Round(time.Second), speedTarget(sm))
		n.saidAt = now
	}
	return false
}

// fetchBenchmarkStatus GETs /inference/benchmark/status. ok=false on any
// error, including a daemon that has no such endpoint.
func fetchBenchmarkStatus(mgmtURL string) (st management.BenchmarkStatusResponse, ok bool) {
	body, err := httpGet(mgmtURL + "/waired/v1/inference/benchmark/status")
	if err != nil {
		return management.BenchmarkStatusResponse{}, false
	}
	if err := json.Unmarshal(body, &st); err != nil {
		return management.BenchmarkStatusResponse{}, false
	}
	return st, true
}

// benchPost performs a status-aware POST: it returns the HTTP status code
// and body separately (unlike httpPost, which collapses non-2xx into an
// error) so the caller can branch on 425 / 404.
func benchPost(ctx context.Context, rawURL string, body []byte) (int, []byte, error) {
	// The benchmark is a mutating verb, so it travels over the local IPC
	// socket like every other write (waired#838) — the loopback TCP port
	// refuses it. No client timeout: a measurement can run for minutes, and
	// ctx is how the caller leaves it.
	target, client, viaSocket, err := mgmtWriteRoute(rawURL, 0)
	if err != nil {
		return 0, nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		if viaSocket {
			return 0, nil, ipcclient.WrapDialError(err)
		}
		return 0, nil, err
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, out, nil
}

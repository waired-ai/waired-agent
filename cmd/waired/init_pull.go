package main

import (
	"encoding/json"
	"io"
	"maps"
	"slices"
	"time"

	"github.com/waired-ai/waired-agent/internal/management"
	"github.com/waired-ai/waired-agent/proto/signer"
)

// modelWaitResult is why the model wait ended.
//
// ready is the bool this used to return. engineFailure is the engine's
// own account of why it is down, and is non-empty ONLY when the wait
// stopped because the engine would not stay up (#310).
//
// Deliberately not "the wait returned false": false is also the honest
// answer on a gateway-only host with inference disabled, on a takeover,
// and when the budget simply ran out. A caller that treated every false
// as a fault would put a warning in front of operators whose machines
// are doing exactly what they were configured to do.
type modelWaitResult struct {
	ready         bool
	engineFailure string
	// pending is the wait ending with local AI still on its way: the
	// model had not finished downloading inside init's window, the
	// engine was still being brought up, or the operator took the
	// terminal back and left the agent to it. Nothing failed and nothing
	// was configured off — the daemon is still working — so this is not
	// engineFailure's weaker sibling and never changes the exit code.
	//
	// Set on exactly the three endings whose own printed line already
	// promises the work continues, and on no other. In particular NOT on
	// the disabled/stopped arm, which is a gateway-only host answering
	// truthfully and silently: keying a caller on plain !ready instead
	// would put a "still setting up" ending in front of every host that
	// was configured never to have local AI (#569).
	pending bool
}

// waitForBundledModel blocks until the agent's active (bundled) model has
// finished downloading and the engine is serving it, rendering a percentage
// progress line as the pull proceeds. It is the foreground half of issue #490:
// on a fresh bundled install the agent pulls the multi-GB model in the
// background right after init starts it, so without this step `waired init`
// would return while the download proceeds invisibly. Polling
// /waired/v1/inference/status — which the agent already populates with
// byte-level Models.Downloads — keeps the agent the single owner of the
// engine/pull; init only watches and renders.
//
// It returns ready once the model is ready, so the post-init benchmark (and
// the issue #133 auto-fallback) can then run immediately. It returns not-ready
// — without holding init hostage — when readiness can't be confirmed (daemon
// unreachable, terminal pull failure, inference disabled/parked, the engine
// never coming up within benchNoEngineGrace or never staying up, or
// benchPollDeadline elapsing). The agent keeps pulling in the background
// regardless, so callers treat not-ready as a soft skip and continue —
// except that engineFailure, when set, is a fault worth reporting (#310).
//
// budget bounds the wait; callers outside the NAVI onboarding path pass
// benchPollDeadline, which keeps their behaviour byte-identical.
// engineComing says an engine can still plausibly appear on this host
// (engineArrivalPending, setup_install.go), which changes exactly one thing:
// the no_engine grace no longer ends the wait, because the executor is about
// to install the very engine that grace was written to give up on. It used to
// be "a browser setup is active" (waired#835 §9) — too wide, because a setup
// whose engine install had just FAILED still qualified, and the terminal then
// waited out the whole setup budget for an engine that was never coming (#188).
// enter (nil = no takeover offer) lets the operator take the terminal back.
// setup (nil = nothing to watch for) reports a browser setup starting
// during the wait — the case awaitSetupBudget's 3-minute grace gave up on
// (#308). This is the longest wait in the flow, so it is where a slow
// operator's browser setup actually lands.
// target (nil = no wizard; key off the active model as before) names the
// model a browser wizard chose (#306). It decides WHICH model this wait
// reports on and changes nothing else: on the host in #306 the terminal
// printed "qwen2.5-coder-14b-instruct ready" and returned while the
// operator's 44 GB choice was still downloading, because subsystem_state
// answers for the ACTIVE model and the agent had picked its own.
func waitForBundledModel(mgmtURL string, out io.Writer, tty bool, budget time.Duration, engineComing bool, enter *enterWatch, setup *setupWatch, target *modelTarget) modelWaitResult {
	if !waitDaemonReachable(mgmtURL, 15*time.Second) {
		// The caller already prints a "start the agent, then …" hint; stay
		// quiet here so we don't double up.
		return modelWaitResult{}
	}
	deadline := time.Now().Add(budget)
	var noEngineDeadline time.Time // armed on no_engine, disarmed once engine is up
	line := downloadLineState{lastPct: -1}
	var rate rateWindow
	dlHinted := false // one-time "this is a multi-GB transfer" hint before the bar
	want := ""        // the wizard's model, once it has named one (#306)
	failedStreak := 0
	var unseenDeadline time.Time    // armed while the wizard's model is on none of the daemon's books
	var engineFailedSince time.Time // armed on the first engine_failed, never disarmed (#310)

	// sawTarget records whether this wait ever had something to wait FOR:
	// a model the daemon named as active, or one it listed as downloading.
	// Latched over the whole wait rather than read off the last snapshot,
	// because one snapshot cannot tell "nothing was ever selected" from
	// "the download just ended" — and the deadline arm below says opposite
	// things about those two (waired-agent#736).
	sawTarget := false

	// lastNote dedups the per-phase step lines: each distinct transitional
	// phase prints one concise note as it is entered, so the user watches
	// the engine move through its phases (start → prepare → download → load
	// → ready) instead of staring at a single silent line. The live download
	// bar uses the stepDownloading sentinel so a following step line
	// terminates the in-place bar exactly once.
	//
	// The key is the rendered SENTENCE rather than the subsystem_state it
	// came from. Once the wait can be keyed to a model the agent is not
	// serving, several states render the same sentence — "awaiting_model"
	// and "loading" both become "Preparing to download <the wizard's
	// model>…" — and a state key would reprint it every time the daemon
	// flapped between them. The question the key answers is "have we said
	// this already?", so the sentence is what it should be.
	lastNote := ""
	announce := func(msg string) {
		if msg == lastNote {
			return
		}
		lastNote = msg
		endProgressLine(out, tty, &line)
		writePrompt(out, msg)
	}

	for {
		// The watches speak through this loop rather than printing for
		// themselves, so their lines land after the in-place progress bar
		// has been terminated — and at most once per tick, however many
		// of them have something to say.
		barEnded := false
		endBar := func() {
			if !barEnded {
				barEnded = true
				endProgressLine(out, tty, &line)
			}
		}

		// #308: the browser setup started after the grace expired. Close
		// the takeover offer (it would strand a setup the control plane
		// believes is running elsewhere), say what this window is doing
		// now, and give the wait the residency budget it would have had if
		// the operator had clicked three minutes sooner.
		if started, setupBudget, coming := setup.Poll(); started {
			endBar()
			enter.Close(out)
			writePrompt(out, setupKeepTerminalOpenLine)
			deadline = time.Now().Add(setupBudget)
			engineComing = coming
			// The engine the grace gave up on is the one the wizard is
			// about to install, so a grace already counting down would
			// otherwise end the wait for it (#188 in reverse).
			noEngineDeadline = time.Time{}
			lastNote = "" // re-announce the current phase after the interruption
		}

		// The takeover exchange (init_takeover.go).
		if took, note := enter.Poll(); note != "" || took {
			endBar()
			if note != "" {
				writePrompt(out, note)
			}
			if took {
				writePrompt(out, "Continuing in the background — the agent finishes the download on its own.")
				// pending: that sentence is the promise, and the closing
				// box has to keep it rather than celebrate (#569).
				return modelWaitResult{pending: true}
			}
			lastNote = "" // re-announce the current phase after the interruption
		}

		// #306: the model the browser wizard chose, which is not
		// necessarily the one the agent decided to serve. Read before the
		// status below, so a new target and the first status keyed to it
		// land on the same tick.
		if got := target.Poll(); got != want {
			endBar()
			if lastNote != "" {
				// Something has already been narrated for the previous
				// keying — in the #308 sequence, a whole progress bar for
				// the agent's own model. The bar is about to restart at a
				// different model's percentage, which unexplained reads as
				// the download going backwards. On the ordinary path this
				// is the first tick, nothing has been said, and the phase
				// note below names the model on its own.
				writePromptf(out, "Now waiting for the model chosen in your browser: %s.\n", got)
			}
			want = got
			// Every piece of render state below describes the model we just
			// stopped waiting for. line most of all: off a TTY
			// drawDownloadLine suppresses a redraw inside the same 10%
			// bucket, so a stale lastPct can swallow the new model's first
			// bar entirely.
			line = downloadLineState{lastPct: -1}
			rate = rateWindow{}
			dlHinted, failedStreak, lastNote = false, 0, ""
			unseenDeadline = time.Time{}
			// noEngineDeadline deliberately survives: engine health is not
			// a property of which model is being waited for.
		}

		st, ok := fetchInferenceStatus(mgmtURL)
		if ok && (want != "" || st.Active != nil || len(st.Models.Downloading) > 0) {
			// A wizard target counts on its own: naming a model IS having
			// something to wait for, whether or not the daemon has taken it
			// up yet — the arms that answer "named but not moving" are
			// further down and say so themselves.
			sawTarget = true
		}
		switch {
		case !ok:
			// /status unreachable this tick — keep waiting, re-read next tick.
		case waitModelReady(st, want):
			endProgressLine(out, tty, &line)
			writePromptf(out, "%s  %s ready\n", emo("✅", "[ok]"), waitModelName(st, want))
			return modelWaitResult{ready: true}
		case waitModelFailed(st, want):
			// One observation is terminal for the agent's own model — that
			// is the pre-#306 behaviour, and what this line's copy promises.
			// It is not terminal for a wizard-chosen one: the agent records
			// `failed` for an in-flight pull as it shuts down, and the
			// post-restart bootstrap picks the same model straight back up
			// (the engine install a wizard is driving is exactly what
			// restarts it). Note this is NOT waitForModelSwitch's reason —
			// applying a desired model never schedules a restart of its own.
			failedStreak++
			if want == "" || failedStreak >= switchFailedStreak {
				endProgressLine(out, tty, &line)
				// Both halves of this line were wrong (waired-agent#328).
				//
				// "the agent will keep retrying" — it does not. #306 gave
				// runPullJob a bounded retry, and this line prints only
				// once that budget is spent; a full disk is deliberately
				// not retried at all. Promising a retry that is not coming
				// is how a wizard-less host sat waiting for nothing.
				//
				// "or `waired runtimes benchmark` later" — that command
				// refuses on exactly this state, by name: its pull_failed
				// arm prints "Model download failed; skipping…". So the
				// failure line recommended the one command guaranteed to
				// bounce. It stays on the TIMEOUT branch below, where the
				// download may yet finish in the background and the
				// recommendation is coherent.
				//
				// engineFailure stays unset: this is the MODEL's failure,
				// and #310's fault channel is for an engine that could not
				// come up. A pull that failed on a healthy engine is a soft
				// skip, which is what the zero value means.
				writePromptf(out, "Model download failed.%s\n",
					reasonSuffix(waitFailureReason(st, want)))
				writePromptf(out, "Retry with `waired models pull %s`; `waired status` and "+
					"`waired doctor` show more.\n", waitModelName(st, want))
				return modelWaitResult{}
			}
		case st.SubsystemState == "engine_failed" ||
			(!engineFailedSince.IsZero() && engineRestarting(st.SubsystemState)):
			// #310: the engine is down — a crashed model runner, an
			// exhausted recovery budget, or a boot that never came up.
			// This state had NO arm at all, so it fell through to
			// `default:` and rendered as "Preparing the model…" for the
			// rest of the budget, while the daemon's crash-recovery cycle
			// flapped to "starting" and back and printed "Engine
			// starting…" in between. Two sentences, alternating, neither
			// of them true, on a host where nothing was ever going to
			// download.
			//
			// `starting` is folded in ONCE a failure has been seen,
			// because the daemon reports `starting` for any restart in
			// flight — its arm sits ahead of the engine_failed one — and
			// the recovery cycle spends its life between the two.
			// Suppressing it before a failure has been seen would take
			// "Engine starting…" away from every ordinary first run.
			//
			// One observation is not terminal: the daemon restarts the
			// engine on a bounded budget and routinely recovers. So this
			// is bounded the way no_engine is bounded, and by the same
			// grace, for the same reason — one restart cycle. The clock
			// is wall time since the FIRST failure, not a streak, and is
			// deliberately never disarmed: a grace that the flap back to
			// `starting` re-armed every other tick would never expire,
			// which is the bug this arm exists to end.
			//
			// Mutation testing will report that clearing it in `default:`
			// — beside the no_engine grace, which is cleared there —
			// kills no test. That mutant is equivalent while the fold
			// above holds: the restart states never reach `default:`
			// once a failure has been seen, so the only route there is a
			// genuine recovery, where re-granting the grace is a defensible
			// behaviour rather than a bug. Left un-cleared for the simpler
			// contract, not because the other reading is wrong.
			//
			// Not suppressed by engineComing, unlike no_engine: an engine
			// still being installed reports no_engine, and engine_failed
			// means the daemon OBSERVED a failure of one that is already
			// there.
			if engineFailedSince.IsZero() {
				engineFailedSince = time.Now()
			}
			if time.Since(engineFailedSince) > benchNoEngineGrace {
				endProgressLine(out, tty, &line)
				writePromptf(out, "The AI engine failed to start, so %s can't download.\n",
					waitModelName(st, want))
				detail := engineFailureDetail(st)
				if detail != "" {
					writePrompt(out, detail)
				}
				writePrompt(out, "Run `waired doctor` for details; `waired status` shows the current state.")
				// Never empty on this path, even when the daemon recorded
				// no reason: engineFailure is what tells the caller a
				// fault happened at all, so letting it fall to "" would
				// hand the quietest hosts back the success box.
				if detail == "" {
					detail = "the AI engine on this device failed to start"
				}
				return modelWaitResult{engineFailure: detail}
			}
			announce("The AI engine won't start; Waired is retrying…")
		case st.SubsystemState == "disabled" || st.SubsystemState == "stopped":
			// Inference won't become ready while disabled / parked — don't block.
			//
			// The zero value, deliberately: not-ready here is the honest
			// answer for a host that was configured to have no local AI,
			// so it is not pending either. This is the arm that decides a
			// gateway-only install still ends on the success box (#569).
			return modelWaitResult{}
		case st.NoModelSelected && want == "":
			// waired-agent#586: the operator's standing choice is "no
			// model" — nothing is coming and nothing failed. The picker
			// (or the init that ran it) already said what this means, so
			// end quietly with the same zero value as the disabled arm.
			// A wizard-named target (want != "") overrides: naming a
			// model IS the choice changing.
			return modelWaitResult{}
		case st.SubsystemState == "no_engine":
			// Engine still being brought up on a fresh bundled install
			// (issue #489): wait it out within the grace, then conclude it
			// won't come up.
			//
			// Except while an engine is still on its way (waired#835 §9):
			// there the executor is about to install the very engine this
			// grace gives up on, so "no engine yet" is the expected state
			// for minutes. Giving up here is what used to cut the
			// terminal's residency to 3 minutes on exactly the hosts this
			// feature exists for — and, once the install has failed,
			// NOT giving up is what cost an hour (#188).
			if noEngineDeadline.IsZero() && !engineComing {
				noEngineDeadline = time.Now().Add(benchNoEngineGrace)
			}
			if !noEngineDeadline.IsZero() && time.Now().After(noEngineDeadline) {
				endProgressLine(out, tty, &line)
				writePrompt(out, "The AI engine still isn't up; Waired keeps bringing it up in the background.")
				writePrompt(out, "Check progress with `waired status`; if it persists, see `waired doctor` or `journalctl -u waired-agent -e`.")
				// pending, not engineFailure: the daemon has not reported a
				// failure, it just has not finished. engineFailure is the
				// arm above, where one was actually observed (#310, #569).
				return modelWaitResult{pending: true}
			}
			// Nothing is pulled before an engine exists — applying a desired
			// model is itself gated on one being present — so the target
			// being on none of the daemon's books says nothing yet. Keeping
			// this cleared is what stops the grace below eating an engine
			// install, which is minutes long.
			unseenDeadline = time.Time{}
			announce("Waiting for the AI engine to start… " +
				dim("(first run installs the engine — this can take a few minutes)"))
		default:
			// Engine is up; a download may be in flight. Disarm the no_engine
			// grace so a later blip re-arms a fresh window instead of expiring.
			noEngineDeadline = time.Time{}
			failedStreak = 0
			if dl, found := waitDownload(st, want); found && dl.TotalBytes > 0 {
				speed := rate.observe(time.Now(), dl.CompletedBytes)
				pct := int(dl.CompletedBytes * 100 / dl.TotalBytes)
				if !dlHinted {
					dlHinted = true
					announce(dim("Downloading the AI model (several GB — this can take a while)."))
				}
				unseenDeadline = time.Time{}
				lastNote = stepDownloading // the bar owns the line; let a later step end it
				drawDownloadLine(out, tty, &line, waitModelName(st, want), pct, dl.CompletedBytes, dl.TotalBytes, speed)
			} else {
				if want != "" && !targetVisible(st, want) {
					// The engine is up and the daemon has this model in no
					// bucket at all. Two of the three ways that happens are
					// now answerable, and only the third needs the grace.
					//
					// A refusal (#404): the reconciler tried to apply the
					// wizard's choice and was told no. That is not going to
					// change by waiting — admission is once per desired
					// value — so say what happened and stop. The daemon has
					// already reported the same refusal to the wizard, so
					// the two surfaces agree.
					if code, detail, ok := target.Refused(); ok {
						endProgressLine(out, tty, &line)
						writeModelRefusal(out, want, code, detail)
						return modelWaitResult{}
					}
					// An id this daemon's catalog does not carry (#403):
					// nothing is coming, and no download will start. Only
					// concluded from a daemon that ANSWERS the question —
					// an empty list is what an older one sends.
					if len(st.Models.NotPresent) > 0 && !slices.Contains(st.Models.NotPresent, want) {
						endProgressLine(out, tty, &line)
						writePromptf(out, "Waired doesn't have a model called %s on this computer.\n", want)
						writePrompt(out, "Run `waired models ls` to see what it does have, "+
							"or pick another model in your browser.")
						return modelWaitResult{}
					}
					// What is left is a pull that has not been dispatched
					// yet — see targetPullGrace. Bounded, then a soft skip:
					// the caller carries on and the agent keeps the work.
					// Never a keystroke, because a browser-driven wait has
					// already withdrawn the takeover offer and there is
					// nothing left to press.
					if unseenDeadline.IsZero() {
						unseenDeadline = time.Now().Add(targetPullGrace)
					}
					if time.Now().After(unseenDeadline) {
						endProgressLine(out, tty, &line)
						writePromptf(out, "Waired hasn't started downloading %s yet; "+
							"it keeps trying in the background.\n", want)
						writePrompt(out, "Check with `waired models ls`, or `waired doctor` if it stays this way.")
						return modelWaitResult{}
					}
				} else {
					unseenDeadline = time.Time{}
				}
				announce(waitPrepMessage(st, want))
			}
		}

		if time.Now().After(deadline) {
			endProgressLine(out, tty, &line)
			if !sawTarget {
				// Nothing was ever selected for this host, so there is no
				// download to hand to the background and no progress for
				// `waired status` to show. Saying otherwise sent operators
				// to watch a transfer that did not exist (waired-agent#736):
				// observed on a host where the selector declined to
				// preselect ("not preselected here") and the only model in
				// the store was the host-cutoff probe.
				//
				// `waired runtimes benchmark` is deliberately NOT offered
				// here — it refuses on exactly this state, the same defect
				// the pull-failure arm above records. `waired models ls`
				// is not offered either: on this host it prints "(no models
				// registered)", which is true and useless. `waired init`
				// would not re-ask, so the two routes that work are the
				// pull and the dashboard — the pairing the picker already
				// uses (init_model_picker.go).
				//
				// Not pending: pending selects the "local AI is still
				// setting up here" box, and nothing is setting up. The
				// disabled and no-model-selected arms above return the same
				// zero value for the same reason; unlike them this one
				// speaks, because the operator has just waited out the
				// whole window.
				writePrompt(out, "No model was chosen for this computer, so nothing is downloading.")
				writePrompt(out, "Pick one with `waired models pull <model>`, or from the browser dashboard.")
				return modelWaitResult{}
			}
			writePrompt(out, "Model still downloading; it will finish in the background. "+
				"Run `waired status` to watch progress, or `waired runtimes benchmark` later to check performance.")
			// pending: this line hands the terminal back, and until #569
			// the caller went straight on to a second readiness wait of
			// its own — up to ten more minutes on the download this
			// sentence had just handed to the background.
			return modelWaitResult{pending: true}
		}
		time.Sleep(pullPollInterval)
	}
}

// writeModelRefusal says the daemon will not run the model the wizard
// chose, and what to do instead (waired-agent#404).
//
// The middle line is the daemon's own words for why, printed as recorded
// — the same treatment engineFailureDetail gets, and for the same reason:
// this is the moment the operator is looking at the terminal, and the
// alternative here was a five-minute wait ending in "hasn't started
// downloading yet", which was true of nothing.
//
// The action line is keyed off the §7 code rather than the prose. Only
// engine_not_ready has a second way out — the model needs a newer engine
// than this computer has, which an update supplies — and that is the
// same advice the wizard gives for the same code.
func writeModelRefusal(out io.Writer, model, code, detail string) {
	writePromptf(out, "Waired can't download %s on this computer.\n", model)
	if detail != "" {
		writePrompt(out, detail)
	}
	if code == signer.SetupErrorEngineNotReady {
		writePrompt(out, "Update Waired here (`waired update`), or pick a different model in your browser.")
		return
	}
	writePrompt(out, "Pick a different model in your browser, or run "+
		"`waired models ls --detail` to see what fits here.")
}

// switchFailedStreak is how many consecutive Failed observations of the
// switch target waitForModelSwitch tolerates before treating the download
// as terminally failed. The agent restart the switch schedules can leave a
// transient failed record behind (a cancelled pull) that the post-restart
// bootstrap immediately retries, so a single observation must not abort
// the wait.
const switchFailedStreak = 3

// waitForModelSwitch blocks until modelID — just accepted via
// /inference/preferred-model — is pulled and ready, tolerating the agent
// restart the accept schedules (status fetches fail for a few seconds;
// keep polling). Unlike waitForBundledModel it keys strictly off modelID
// in Models.Ready / Models.Failed / Models.Downloads, NOT st.Active: the
// switch target only becomes the active model once its pull completes.
// enter (inert = no backgrounding) lets the user press Enter to leave the
// download running in the background.
// Returns true once the model is ready and serving.
func waitForModelSwitch(mgmtURL, modelID string, out io.Writer, tty bool, enter *enterWatch) bool {
	deadline := time.Now().Add(benchPollDeadline)
	line := downloadLineState{lastPct: -1}
	var rate rateWindow
	label := bundledModelLabelDefault(modelID)
	failedStreak := 0
	dlHinted := false
	var engineFailedSince time.Time // see waitForBundledModel's arm (#310)

	lastStep := ""
	announce := func(step, msg string) {
		if step == lastStep {
			return
		}
		lastStep = step
		endProgressLine(out, tty, &line)
		writePrompt(out, msg)
	}

	for {
		if fired, _ := enter.Poll(); fired {
			endProgressLine(out, tty, &line)
			printSwitchBackgroundNote(out, label)
			return false
		}

		st, ok := fetchInferenceStatus(mgmtURL)
		switch {
		case !ok:
			// The accept schedules an immediate agent restart, so the
			// management API is briefly unreachable — expected, keep polling.
			announce("restart", "Waiting for the agent to restart…")
		case slices.Contains(st.Models.Ready, modelID):
			endProgressLine(out, tty, &line)
			writePromptf(out, "%s  %s ready — the agent is now serving it.\n", emo("✅", "[ok]"), label)
			return true
		case slices.Contains(st.Models.Failed, modelID):
			failedStreak++
			if failedStreak >= switchFailedStreak {
				endProgressLine(out, tty, &line)
				// The benchmark mention stays here, unlike the setup-wait
				// line above: this branch is reached FROM `waired runtimes
				// benchmark`, and re-running it re-offers the switch.
				writePromptf(out, "Download failed.%s\nCheck `waired models ls`; retry with "+
					"`waired models pull %s` or re-run `waired runtimes benchmark`.\n",
					reasonSuffix(waitFailureReason(st, modelID)), modelID)
				return false
			}
		case st.SubsystemState == "engine_failed" ||
			(!engineFailedSince.IsZero() && engineRestarting(st.SubsystemState)):
			// #310. This function reads no subsystem state at all today,
			// so a dead engine costs it the full benchPollDeadline saying
			// "Preparing to download …" — ten minutes for a download that
			// has nothing to run on. Same shape and same grace as
			// waitForBundledModel's arm; see it for why `starting` folds
			// in only after a failure and why the clock is never disarmed.
			//
			// `disabled` / `stopped` deliberately stay unhandled here: the
			// accept this wait follows schedules an agent restart, so the
			// states either side of that window need a judgement this
			// change does not have to make.
			if engineFailedSince.IsZero() {
				engineFailedSince = time.Now()
			}
			if time.Since(engineFailedSince) > benchNoEngineGrace {
				endProgressLine(out, tty, &line)
				writePromptf(out, "The AI engine failed to start, so %s can't download.\n", label)
				if detail := engineFailureDetail(st); detail != "" {
					writePrompt(out, detail)
				}
				writePrompt(out, "Run `waired doctor` for details; `waired status` shows the current state.")
				return false
			}
			announce("engine_failed", "The AI engine won't start; Waired is retrying…")
		default:
			failedStreak = 0
			if dl, found := downloadFor(st, modelID); found && dl.TotalBytes > 0 {
				speed := rate.observe(time.Now(), dl.CompletedBytes)
				pct := int(dl.CompletedBytes * 100 / dl.TotalBytes)
				if !dlHinted {
					dlHinted = true
					announce("download_hint", dim("Downloading the AI model (several GB — this can take a while)."))
				}
				lastStep = stepDownloading // the bar owns the line; let a later step end it
				drawDownloadLine(out, tty, &line, label, pct, dl.CompletedBytes, dl.TotalBytes, speed)
			} else {
				announce("preparing", "Preparing to download "+label+"…")
			}
		}

		if time.Now().After(deadline) {
			endProgressLine(out, tty, &line)
			printSwitchBackgroundNote(out, label)
			return false
		}
		time.Sleep(pullPollInterval)
	}
}

// printSwitchBackgroundNote tells the user what happens after the
// foreground wait stops watching a switch download (Enter pressed or the
// deadline elapsed): the agent owns the pull and will start serving the
// model on its own.
func printSwitchBackgroundNote(out io.Writer, label string) {
	writePromptf(out, "Continuing in the background — the agent will finish the download and start serving %s when it's ready.\n", label)
	writePrompt(out, "Check progress with `waired models ls` or `waired status`.")
}

// downloadFor returns the in-flight download entry for modelID; found is
// false when no sized progress has been reported for it yet.
func downloadFor(st management.InferenceStatus, modelID string) (management.ModelDownload, bool) {
	for _, d := range st.Models.Downloads {
		if d.Model == modelID {
			return d, true
		}
	}
	return management.ModelDownload{}, false
}

// pullPollInterval is the gap between /inference/status polls while init
// watches the model download. Deliberately tighter than benchPollInterval:
// the bar redraws — and the rate re-samples — once per poll, and at 3 s the
// line sat visually unchanged long enough to read as frozen (the byte
// counts only tick every 0.1 GB). A var so tests can shrink it.
var pullPollInterval = 1 * time.Second

// rateWindowSpan is how far back rateWindow smooths the download rate.
const rateWindowSpan = 5 * time.Second

// rateWindow smooths the polled download rate over a short rolling window
// of (time, bytes) samples, so 1 s polls don't make the displayed number
// jitter — or vanish whenever a single poll happens to see no byte
// movement (the old two-poll delta did exactly that, stripping the rate
// off the bar). observe returns -1 until samples span time (rate unknown
// yet) and the windowed average afterwards — 0 during a genuine stall,
// which drawDownloadLine renders as "(0 B/s)" so a stalled transfer looks
// different from a frozen UI. A byte regression (the agent restarted the
// pull) resets the window.
type rateWindow struct {
	samples []rateSample
}

type rateSample struct {
	at    time.Time
	bytes int64
}

func (w *rateWindow) observe(now time.Time, completed int64) int64 {
	if n := len(w.samples); n > 0 && completed < w.samples[n-1].bytes {
		w.samples = w.samples[:0]
	}
	w.samples = append(w.samples, rateSample{at: now, bytes: completed})
	// Drop samples that fell out of the window, but keep one older sample
	// as the anchor — pruning to a single sample would flip a long stall
	// back to "unknown" instead of decaying the rate to 0.
	cutoff := now.Add(-rateWindowSpan)
	for len(w.samples) > 1 && w.samples[1].at.Before(cutoff) {
		w.samples = w.samples[1:]
	}
	first, last := w.samples[0], w.samples[len(w.samples)-1]
	secs := last.at.Sub(first.at).Seconds()
	if secs <= 0 {
		return -1
	}
	return int64(float64(last.bytes-first.bytes) / secs)
}

// stepDownloading is the lastStep sentinel used while the live download bar
// owns the output line; it is not a real subsystem_state, so the next step
// note always differs from it and terminates the bar.
const stepDownloading = "__downloading__"

// prepMessage maps a non-terminal subsystem_state (engine up, model not yet
// downloading bytes) to a concise one-line step note, so the pre-download
// phases announce themselves instead of waiting silently.
func prepMessage(st management.InferenceStatus) string {
	switch st.SubsystemState {
	case "initializing":
		return "Starting the AI engine…"
	case "starting":
		return "Engine starting…"
	case "loading":
		return "Loading " + activeModelName(st) + "…"
	case "awaiting_model":
		return "Preparing to download " + activeModelName(st) + "…"
	case "degraded":
		return "Using a fallback AI engine…"
	default:
		return "Preparing the model…"
	}
}

// endProgressLine terminates an in-place TTY progress line with a newline so a
// following message starts on its own line. No-op when nothing was drawn or
// off a TTY (there the progress lines already end in newlines).
func endProgressLine(out io.Writer, tty bool, st *downloadLineState) {
	if tty && st.lastPct >= 0 {
		writePrompt(out)
	}
}

// fetchInferenceStatus GETs /inference/status and decodes it; ok is false on
// any transport / decode error so callers can keep polling.
func fetchInferenceStatus(mgmtURL string) (st management.InferenceStatus, ok bool) {
	body, err := httpGet(mgmtURL + "/waired/v1/inference/status")
	if err != nil {
		return management.InferenceStatus{}, false
	}
	if err := json.Unmarshal(body, &st); err != nil {
		return management.InferenceStatus{}, false
	}
	return st, true
}

// modelReady reports whether the active model is downloaded and serving — the
// subsystem reports "ready", or (defensively, e.g. a degraded fallback engine)
// the active model id is in the ready set.
func modelReady(st management.InferenceStatus) bool {
	if st.SubsystemState == "ready" {
		return true
	}
	return st.Active != nil && slices.Contains(st.Models.Ready, st.Active.ModelID)
}

// modelFailed reports whether the active model's most recent download failed.
func modelFailed(st management.InferenceStatus) bool {
	return st.Active != nil && slices.Contains(st.Models.Failed, st.Active.ModelID)
}

// engineRestarting names the states the daemon reports while it is
// bringing the engine back up. Only ever consulted after a failure has
// been seen (#310): before that they are an ordinary engine start and
// keep their own narration.
//
// Both are here because the daemon's own derivation puts `starting`
// ahead of `engine_failed` — any EnsureRunning in flight reports
// `starting` — so a crashing engine spends most of its ticks in one of
// these two rather than in the failure state itself.
func engineRestarting(state string) bool {
	return state == "starting" || state == "initializing"
}

// engineFailureDetail is the engine's own account of why it is down, in
// the shape `waired status` and the tray already print it: "<name>:
// <reason>". "" when no runtime recorded one — an older daemon, or a
// failure the adapter could not describe — so callers must not print a
// bare "ollama: " for it.
//
// The daemon folds the engine's stderr tail into that reason as it
// happens, so this is routinely multi-line and can run to kilobytes. It
// is printed whole: this is the one moment the operator is looking at
// the terminal, and the alternative on the host that prompted #310 was
// no information at all.
//
// Sorted rather than ranged: the line an operator reads must not depend
// on Go's map iteration order.
func engineFailureDetail(st management.InferenceStatus) string {
	for _, name := range slices.Sorted(maps.Keys(st.Runtimes)) {
		if r := st.Runtimes[name]; r.State == "failed" && r.LastError != "" {
			return name + ": " + r.LastError
		}
	}
	return ""
}

// activeModelName is the active model id, or a generic label before one is set.
func activeModelName(st management.InferenceStatus) string {
	if st.Active != nil && st.Active.ModelID != "" {
		return st.Active.ModelID
	}
	return "the model"
}

// activeDownload returns the in-flight download for the active model, falling
// back to the first in-flight download (the bundled pull is the only one at
// install time). ok is false when no sized download is in progress yet.
func activeDownload(st management.InferenceStatus) (management.ModelDownload, bool) {
	if st.Active != nil {
		for _, d := range st.Models.Downloads {
			if d.Model == st.Active.ModelID {
				return d, true
			}
		}
	}
	if len(st.Models.Downloads) > 0 {
		return st.Models.Downloads[0], true
	}
	return management.ModelDownload{}, false
}

// The wait* helpers below are the whole of #306 inside this file. Given
// want == "" they delegate to the functions above, so every non-wizard
// path stays byte-identical; given a want they key on the model the
// BROWSER WIZARD chose rather than on st.Active.
//
// The split exists because subsystem_state describes the active model and
// nothing else: the daemon derives it entirely from state.Active, and
// state.Active is only committed once a model's weights are Ready. So for
// the whole of a download the operator's choice is invisible to it, while
// "ready" and "pull_failed" answer for a model they did not pick. On the
// host in #306 both were true of a 9 GB model the agent had chosen for
// itself, and the terminal announced it as ready and returned while the
// operator's 44 GB choice was still coming down.

// waitModelReady reports whether the model this wait is keyed to is on
// disk and being served.
func waitModelReady(st management.InferenceStatus, want string) bool {
	if want == "" {
		return modelReady(st)
	}
	// Deliberately not st.SubsystemState == "ready": that is the active
	// model's answer, and the point is that a different model is active.
	return slices.Contains(st.Models.Ready, want)
}

// waitModelFailed reports whether that model's most recent download failed.
func waitModelFailed(st management.InferenceStatus, want string) bool {
	if want == "" {
		return st.SubsystemState == "pull_failed" || modelFailed(st)
	}
	// Again not the subsystem state: "pull_failed" is the active model's
	// download failing, which says nothing about the one being waited on.
	return slices.Contains(st.Models.Failed, want)
}

// waitModelName is the name to print for the model being waited on. It is
// the raw catalog id on both paths, which is what `waired models ls` shows
// and — load-bearing — what the failure line interpolates into a
// copy-pasteable `waired models pull <id>`. A display label there would
// split into two shell arguments, neither of them an alias of anything.
func waitModelName(st management.InferenceStatus, want string) string {
	if want == "" {
		return activeModelName(st)
	}
	return want
}

// reasonSuffix renders a stored failure reason as a sentence tail, or ""
// when there is none. Callers concatenate unconditionally, so a daemon
// that recorded nothing degrades to the bare line it printed before
// rather than to a dangling colon.
func reasonSuffix(reason string) string {
	if reason == "" {
		return ""
	}
	return " " + reason
}

// waitFailureReason is the daemon's stored reason for the model being
// waited on, or "" when it did not record one (an older daemon, or a
// failure written before the reason was kept).
//
// Same want=="" / want!="" split as waitModelFailed, for the same reason:
// with no wizard-chosen target the active model is the one that failed.
func waitFailureReason(st management.InferenceStatus, want string) string {
	target := want
	if target == "" {
		if st.Active == nil {
			return ""
		}
		target = st.Active.ModelID
	}
	for _, f := range st.Models.Failures {
		if f.Model == target {
			return f.Error
		}
	}
	return ""
}

// waitDownload is the in-flight transfer to render.
func waitDownload(st management.InferenceStatus, want string) (management.ModelDownload, bool) {
	if want == "" {
		return activeDownload(st)
	}
	// Exact match, never activeDownload's Downloads[0] fallback: that
	// fallback picks an arbitrary one of two concurrent pulls, which is how
	// the bar came to count one model's bytes under another's name.
	return downloadFor(st, want)
}

// waitPrepMessage is prepMessage with the model-naming arms retargeted:
// keep the arms that describe the ENGINE, replace the ones that name a
// model, since every one of those interpolates the active model.
func waitPrepMessage(st management.InferenceStatus, want string) string {
	if want == "" {
		return prepMessage(st)
	}
	switch st.SubsystemState {
	case "initializing", "starting":
		return prepMessage(st)
	default:
		return "Preparing to download " + want + "…"
	}
}

// targetVisible reports whether the daemon has WORK under way for the
// wait's model — on disk, coming down, or failed trying.
//
// It is deliberately not "the daemon knows this model": since #403 the
// snapshot answers that separately in Models.NotPresent, and the two
// questions have different callers. This one decides whether there is
// anything to render; that one tells an id nothing has started on apart
// from an id this build has never heard of.
func targetVisible(st management.InferenceStatus, want string) bool {
	if slices.Contains(st.Models.Ready, want) ||
		slices.Contains(st.Models.Downloading, want) ||
		slices.Contains(st.Models.Failed, want) {
		return true
	}
	_, ok := downloadFor(st, want)
	return ok
}

// targetPullGrace bounds how long the wait tolerates a wizard-chosen model
// the daemon has not started working on.
//
// It exists because the daemon could refuse a desired model permanently
// with no way to say so here: admission is once per desired value and the
// applied flag is set BEFORE the attempt, so a refusal was never retried
// and was recorded for the control plane only. Without this the wait
// would sit out a wizard's eight-hour residency budget saying "Preparing
// to download…" for a download nobody was going to start.
//
// Both of those refusals are now reported — /setup/state carries the code
// (#404) and the snapshot names the models nothing has started on (#403)
// — and the wait answers them before it reaches this grace. What is left
// for the grace is the honest transient: the reconciler has accepted the
// instruction and not dispatched the pull yet, and a leftover instruction
// read as live by a daemon too old to send desired_stale. A var so tests
// can shrink it.
var targetPullGrace = 5 * time.Minute

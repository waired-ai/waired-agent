package main

import (
	"encoding/json"
	"io"
	"time"

	"github.com/waired-ai/waired-agent/internal/management"
	"github.com/waired-ai/waired-agent/internal/runtime/state"
)

// Owner-ruled install-flow step 6 (2026-08-08, waired-ai/waired#1067;
// waired-agent#585): the #496 host-speed cutoff used to default local
// inference off silently on the daemon path, with the terminal sitting
// right there. Between the engine install and the model wait, init now
// waits for the one-time measurement and, when it misses the budget,
// asks — default off. The silent daemon default stays for unattended
// paths: it is the same default, applied with nobody to ask.

var (
	// hostSpeedAskWait bounds how long init waits for the measurement.
	// Generous because what it waits for is the ~1 GB probe model's own
	// download plus three samples (#496) — and the multi-GB model wait
	// behind it dwarfs it either way. Vars so tests do not wait.
	hostSpeedAskWait = 20 * time.Minute
	hostSpeedAskPoll = 2 * time.Second
	// hostSpeedNarrateEvery is how often the wait says it is still going
	// (waired-agent#623). 30 s is short enough that the gap never reads as a
	// hang and long enough that a six-minute measurement produces a dozen
	// lines rather than a wall of them.
	hostSpeedNarrateEvery = 30 * time.Second
)

// hostSpeedPoll is one status read: the measurement (with the
// TurnedInferenceOff claim already cross-checked against the toggle,
// exactly as fetchHostSpeed does) plus the two states the guards read.
type hostSpeedPoll struct {
	hs              *management.HostSpeedStatus
	desiredState    string
	desiredStateSet bool
	subState        string
	stage           string
	// engineErr is the serving engine's own reason when it cannot start,
	// in the form the two waits after this one already print
	// (waired-agent#1134). Empty when no engine is reporting one.
	engineErr string
	ok        bool
}

// hostSpeedStageGaveUp reports whether the measurement stopped without a
// figure. It is what ends the wait for a re-measurement that is never
// going to arrive.
//
// "measured" is NOT in the list, and that is the point: the daemon
// reports it for a figure merely STORED, not one taken just now
// (setupHostSpeedProgress, waired#1143), so a re-run polling a host with
// a stale figure sees "measured" from its first read. What a fresh figure
// landed is answered by measured_at changing, not by this.
//
// Anything else — an empty stage from an older daemon, "measure_deferred"
// from one whose engine another measurement had taken — means keep
// waiting, bounded by hostSpeedAskWait as before. A host too busy to be
// measured therefore spends that budget and then judges on what it has,
// which is the fail-open end the whole step already lands on.
//
// The daemon reported that second case as "measure_failed" until
// waired-agent#579, so this list ended the wait on a host whose engine was
// merely busy for a moment — the case the sentence above says to wait
// through. The list itself did not change; the daemon stopped calling a
// deferral a failure.
func hostSpeedStageGaveUp(stage string) bool {
	switch stage {
	case "probe_failed", "measure_failed":
		return true
	default:
		return false
	}
}

func readHostSpeedPoll(mgmt string) hostSpeedPoll {
	body, err := httpGet(mgmt + "/waired/v1/inference/status")
	if err != nil {
		return hostSpeedPoll{}
	}
	var s inferenceStatusResponse
	if err := json.Unmarshal(body, &s); err != nil {
		return hostSpeedPoll{}
	}
	if hostSpeedFigure(s.HostSpeed) == "" {
		// Nothing to report yet. A screened host fills TurnFloorSeconds
		// and leaves TurnSeconds at zero (waired-agent#579), so this asks
		// for "is there a figure" rather than for one of the two fields —
		// checking TurnSeconds alone would park step 6 on the full wait
		// for a host that had already been judged.
		s.HostSpeed = nil
	}
	if s.HostSpeed != nil && s.DesiredState != string(state.InferenceDisabled) {
		s.HostSpeed.TurnedInferenceOff = false
	}
	return hostSpeedPoll{
		hs: s.HostSpeed, desiredState: s.DesiredState, desiredStateSet: s.DesiredStateSet,
		subState: s.SubsystemState, stage: s.HostSpeedStage, ok: true,
		engineErr: engineFailureDetail(management.InferenceStatus{Runtimes: s.Runtimes}),
	}
}

// requestHostSpeedRemeasure asks the daemon to re-take the install-time
// measurement (waired-agent#599). Returns whether one was started, which
// only the tests read — the flow behaves the same either way, because a
// declined request means the figure it is about to wait for is already this
// install's own.
func requestHostSpeedRemeasure(mgmt string) bool {
	body, err := httpPost(mgmt+"/waired/v1/inference/host-speed/remeasure", nil)
	if err != nil {
		return false
	}
	var resp management.HostSpeedRemeasureResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return false
	}
	return resp.Started
}

// confirmHostSpeedBudget runs step 6. It never fails an install: every
// early return leaves the daemon exactly as it was, and the #133
// post-download benchmark still runs afterwards as the confirmation
// measurement.
//
// keptOn reports whether local AI is (as far as this step can tell)
// still on when it returns — false exactly when it observed or applied
// an off state. The model picker behind it (waired-agent#586) keys on
// this: asking which model to download right after local AI went off
// would be the flow contradicting itself. Unknown states (an unreachable
// daemon, a measurement that never came) stay true — the picker has its
// own fail-open guards.
//
// stillMine reports whether this terminal still owns the questions — a
// browser setup that started during the wait takes them over (§4.2),
// and a prompt racing the wizard is what it must never print.
func confirmHostSpeedBudget(mgmtURL string, inf daemonInitInference, nonInteractive bool, sc lineReader, out io.Writer, stillMine func() bool) (keptOn bool) {
	if inf.Enabled != nil && *inf.Enabled {
		// #465: the operator already answered with --inference-enabled=true.
		// The daemon's cutoff does not override a set toggle, and neither
		// may this ask.
		return true
	}

	// Ask for a fresh figure before waiting for one. A re-run of `waired
	// init` replays the whole install conversation, benchmarks and gates
	// included (owner ruling 2026-08-09, waired-agent#599), and the stored
	// figure is otherwise kept for the life of the install — which is how
	// three machines came to be carrying a measurement that described a
	// resident model rather than a host, with no way to retake it short of
	// an upgrade (waired#1140).
	//
	// Best-effort and ignored on every failure, like every other read of
	// this route: an older daemon answers 404, and the wait below then
	// behaves exactly as it did before. The daemon declines by itself on a
	// fresh install, where it measured seconds ago.
	//
	// The figure standing on disk before the ask is remembered, because
	// the wait below has to be able to tell it from the one being taken
	// now. It could not, and that is how this step came to decide on a
	// stale number while the fresh one landed 44 s later — with both
	// running at once, which is the contention waired-agent#703 is about.
	//
	// A STRING comparison, not a timestamp one: the CLI and the daemon
	// each have their own clock, and "did this change" is the question
	// being asked. Same shape as the engine-version match that decides
	// whether a stored figure still applies.
	//
	// This read IS the loop's first poll, taken before the loop rather
	// than inside it, so asking the question costs no extra request. The
	// loop therefore polls at its END.
	p := readHostSpeedPoll(mgmtURL)
	staleMeasuredAt := ""
	if p.ok && p.hs != nil {
		staleMeasuredAt = p.hs.MeasuredAt
	}
	awaitingFresh := requestHostSpeedRemeasure(mgmtURL)

	deadline := time.Now().Add(hostSpeedAskWait)
	var engineFailedSince time.Time // see the engine_failed arm below (#1134)
	narrated := false
	var narratedAt, saidAt time.Time
	misses, looks := 0, 0
	for {
		if !p.ok {
			// An unreachable daemon — or an older build without the
			// status route — is not going to produce a measurement.
			// Three consecutive misses and this gives up rather than
			// parking init on the full wait (best-effort, like every
			// other read of this route).
			if misses++; misses >= 3 {
				return true
			}
		} else {
			misses = 0
		}
		if p.ok {
			if p.desiredState == string(state.InferenceDisabled) &&
				(p.hs == nil || !p.hs.TurnedInferenceOff) {
				// A person (or the step-4 decline) turned local AI off.
				// That is an answer, not a measurement — nothing to ask.
				return false
			}
			if p.subState == "no_engine" || p.subState == "stopped" {
				// Nothing is going to measure anything.
				return false
			}
			// The engine is down. "engine_failed" is neither of the two
			// states above, so this loop kept waiting: a host whose engine
			// cannot start spent the whole twenty-minute budget saying
			// "still measuring" and then fell open, while the document
			// being polled every two seconds carried the reason
			// (waired-agent#1134).
			//
			// The two waits AFTER this one already stop and say why —
			// init_pull.go's engine_failed arm and init_benchmark.go's
			// "The inference engine could not start." — and this step runs
			// before both (login_client.go calls confirmHostSpeedBudget
			// before waitForBundledModel), so the twenty minutes burned
			// before init reached the one wait that would have named the
			// engine.
			//
			// Bounded, not immediate, and by the same grace and the same
			// arithmetic as those two: the daemon restarts the engine on a
			// budget and routinely recovers, its recovery cycle flaps
			// between `engine_failed` and `starting`, and a first sighting
			// is not a verdict (waired-agent#310). Armed on the first
			// failure and never disarmed, so a flap back to `starting`
			// cannot keep re-arming a grace that then never expires.
			if p.subState == "engine_failed" ||
				(!engineFailedSince.IsZero() && engineRestarting(p.subState)) {
				if engineFailedSince.IsZero() {
					engineFailedSince = time.Now()
				}
				if time.Since(engineFailedSince) > benchNoEngineGrace {
					writePromptf(out, "%s The inference engine couldn't start.%s\n",
						emo("⚠", "!"), reasonSuffix(p.engineErr))
					return false
				}
			}
			// A figure is enough UNLESS one was just asked for and this is
			// still the old one. Waiting for the ask to land is the whole
			// point of making it: a re-run replays the install
			// conversation, and a gate that answers from the previous
			// run's number has replayed nothing (owner ruling 2026-08-09,
			// waired-agent#599).
			fresh := !awaitingFresh || p.hs == nil || p.hs.MeasuredAt != staleMeasuredAt
			if p.hs != nil && fresh {
				break
			}
			// The measurement stopped without producing a new figure —
			// the probe model would not download, the reading was
			// discarded. There is nothing further to wait for, so fall
			// through to whatever is on disk rather than spending the
			// rest of the budget in silence.
			if awaitingFresh && !fresh && hostSpeedStageGaveUp(p.stage) {
				break
			}
			// The same end, for a host that has never been measured. The
			// arm above cannot reach it: with no stored figure p.hs is nil,
			// so `fresh` is true by its own second disjunct and `!fresh` is
			// never satisfied — which made hostSpeedStageGaveUp unreachable
			// on exactly the host `waired init` runs on (waired-agent#1134).
			//
			// There is no staleness question to ask here. Nothing is on
			// disk, the measurement has stopped, so waiting is over
			// whether or not one was asked for.
			if p.hs == nil && hostSpeedStageGaveUp(p.stage) {
				break
			}
			// Announce the wait, but not before there is one. A host
			// holding a figure and waiting only for a FRESHER one is one
			// poll away from having it whenever the daemon declined or had
			// already finished, and a line about minutes of work in front
			// of a five-millisecond gap is noise. A host with no figure at
			// all is waiting from this instant and says so.
			if !narrated && (p.hs == nil || looks > 0) {
				writePromptf(out, "%s Benchmarking this computer with a small model. One time, a few minutes...\n",
					emo("⏱", "*"))
				narrated, narratedAt, saidAt = true, time.Now(), time.Now()
			} else if narrated && time.Since(saidAt) >= hostSpeedNarrateEvery {
				// The wait is deliberate (waired#1099: measure before
				// recommending), but a one-shot line in front of it cannot
				// tell a working measurement from a hung one. Six minutes
				// and forty-five seconds of it were captured through a PTY
				// on a real install, with no spinner, no heartbeat and no
				// bytes (waired-agent#623) — and the wait is longest on
				// exactly the hosts this measurement exists to identify.
				//
				// A plain line rather than a spinner: this output is read
				// through script(1) and CI transcripts as often as by a
				// person, and elapsed time is the thing that distinguishes
				// slow from stuck.
				writePromptf(out, "   still measuring, %s so far\n",
					time.Since(narratedAt).Round(time.Second))
				saidAt = time.Now()
			}
		}
		if time.Now().After(deadline) {
			return true
		}
		time.Sleep(hostSpeedAskPoll)
		p = readHostSpeedPoll(mgmtURL)
		looks++
	}

	hs := p.hs
	if !hostSpeedMissesBudget(hs) {
		return true // within budget (or an older daemon with no budget figure)
	}
	if !stillMine() {
		return true
	}
	// The judgement leads on both arms below, because both are about to
	// act on it — one by asking, one by announcing what it did.
	// `waired inference status` puts the state first instead; that command
	// was asked for the state. (Owner-approved copy, 2026-08-09,
	// waired-agent#579.)
	if nonInteractive {
		writePrompt(out, hostSpeedBelowSpecLine)
		for _, line := range hostSpeedComparisonLines(hs, "  ") {
			writePrompt(out, line)
		}
		// A toggle somebody wrote is an answer, and there is nobody here to
		// ask whether they meant it. Interactive re-runs still ask — a
		// re-run replays the whole install conversation, gates included
		// (owner ruling 2026-08-09, waired-agent#599) — but the ruling is
		// about asking, and this arm cannot ask.
		//
		// TurnedInferenceOff excludes the one "written" toggle nobody chose:
		// the cutoff's own silent default. That is the case step 6 exists
		// for, and it must keep reaching the arm below.
		//
		// #465 / waired#1056 is the rule this restores — the daemon's cutoff
		// has always stood down on a written toggle (hostCutoffIsStillOurs),
		// and until waired#1142 this step could not tell a written one from
		// the live default, so it honoured only "off".
		if hostSpeedWrittenToggleWins(p, hs) {
			writePromptf(out, "%s Non-interactive: leaving local\ninference on, because it was turned on here. "+
				"Turn it off with `waired inference off`.\n", hostSpeedNotRecommendedLine)
			return true
		}
		writePromptf(out, "%s Non-interactive: turning local\ninference off. Turn it back on with `waired inference on`.\n",
			hostSpeedNotRecommendedLine)
		if !hs.TurnedInferenceOff {
			turnLocalAIOff(mgmtURL, out)
		}
		return false
	}

	writePromptf(out, "\n%s %s\n", emo("🐢", "!"), hostSpeedBelowSpecLine)
	for _, line := range hostSpeedComparisonLines(hs, "     ") {
		writePrompt(out, line)
	}
	writePromptf(out, "   %s\n", hostSpeedNotRecommendedLine)
	// Two-line question so the default and the "No disables it" clarifier
	// read as one prompt, the tinyBenchmarkDisableFlow shape.
	q := "Keep local inference on anyway?\n" +
		"  No turns local inference off. This computer still routes requests to your other computers."
	switch ynAsk(out, sc, q, false) {
	case ynYes:
		if hs.TurnedInferenceOff {
			// Overturn the cutoff's silent default: the person just chose.
			if _, err := httpPost(mgmtURL+"/waired/v1/inference/enable", nil); err != nil {
				writePromptf(out, "Warning: couldn't turn local inference back on (%v). Run `waired inference on`.\n", err)
			}
		}
		return true
	case ynNoAnswer:
		// Stdin ended before an answer arrived, so this is the situation
		// the arm above was written for after all — nobody is here to be
		// asked — and it reaches the same decision, through the same
		// predicate (waired-agent#1071).
		//
		// Reaching the DEFAULT instead is what this fixes. A host whose
		// operator turned local inference on would have been switched off
		// here while `--non-interactive` on the same host left it on and
		// said why, which made stating the unattended intent the safer of
		// the two invocations (waired#1142's rule, honoured on one arm).
		//
		// The flag arm's copy is deliberately not reused, unlike
		// noAnswerKeeps (init_benchmark.go, waired-agent#754): its lines
		// carry hostSpeedNotRecommendedLine, which this arm has already
		// printed four lines above, and they name a mode this run is not in.
		writePrompt(out)
		if hostSpeedWrittenToggleWins(p, hs) {
			writePrompt(out, "No answer on stdin. Leaving local inference on, because it was turned on here.")
			writePrompt(out, "Turn it off with `waired inference off`.")
			return true
		}
	}
	if !hs.TurnedInferenceOff {
		turnLocalAIOff(mgmtURL, out)
	}
	writePrompt(out, "Local inference is off. This computer still routes requests to your other computers.")
	return false
}

// hostSpeedWrittenToggleWins reports whether an UNATTENDED answer to step
// 6 has to leave local inference on: somebody wrote this host's toggle,
// and it was not the cutoff's own silent default.
//
// One predicate rather than the condition spelled out on each arm, for
// the reason waired-agent#1051 gives about a rule two surfaces have to
// agree on. It was implemented on the `--non-interactive` arm alone
// (waired#1142), and the interactive arm reached its default on an
// exhausted stdin — so the same host answered "keep it on" to the
// explicit unattended invocation and "turn it off" to the implicit one
// (waired-agent#1071).
//
// Not consulted when a PERSON answers: a re-run replays the whole install
// conversation, gates included (owner ruling 2026-08-09,
// waired-agent#599), and their answer outranks what is on disk.
//
// TurnedInferenceOff excludes the one "written" toggle nobody chose: the
// cutoff's own silent default. That is the case step 6 exists for, and it
// must keep reaching the turn-off tail. #465 / waired#1056 is the rule
// underneath — the daemon's cutoff has always stood down on a written
// toggle (hostCutoffIsStillOurs).
func hostSpeedWrittenToggleWins(p hostSpeedPoll, hs *management.HostSpeedStatus) bool {
	return p.desiredStateSet && hs != nil && !hs.TurnedInferenceOff
}

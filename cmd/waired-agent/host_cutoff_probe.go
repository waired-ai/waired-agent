// The install-time host cutoff's MEASUREMENT half (#496). The policy it
// feeds — what the numbers mean and where the threshold comes from —
// lives in proto/hostfit/host_cutoff.go; the wiring that acts on the
// verdict lives in host_cutoff.go.
//
// Requests go to the ollama NATIVE API: /api/generate is the only surface
// that reports prompt_eval_* / eval_* counters, and the counters are the
// whole point. Wall clock on a 1 GB model is dominated by model load and
// request overhead.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"time"

	"github.com/waired-ai/waired-agent/proto/hostfit"
	"github.com/waired-ai/waired-agent/proto/signer"
)

const (
	// hostCutoffCalibrationLines is how many filler lines the probe sends
	// to find out what a line costs on this model, before it builds the
	// real prompt.
	//
	// It measures rather than assumes because a baked-in figure has now
	// been wrong twice, both times silently and both times in the
	// direction that flatters a slow host. The line is dense in digits and
	// hyphens, so the exchange rate moves with the tokenizer (the #625
	// harness's model reads 35 tokens/line where the probe model reads
	// ~56) AND with the nonce, which repeats on every line: a 19-character
	// nonce measures ~38 where a 32-character one measures ~56. A prompt
	// at 68 % of the requested depth reads FASTER than the same host at
	// full depth (833 vs 671 tok/s on the reference machine), so getting
	// this wrong lets through exactly the hosts the cutoff exists to
	// catch — or, once the depth readback guard is in place, reaches no
	// verdict at all on hosts that are perfectly fine.
	//
	// 50 lines is ~2.8k tokens: about 4 s on the slowest host expected and
	// a fraction of a second on a card. The nonce it uses is the same
	// WIDTH as the sampling nonces (hostCutoffNonce), because the width is
	// what the answer depends on, and a different one so the calibration
	// prompt is not a prefix of the real one.
	hostCutoffCalibrationLines = 50

	// hostCutoffPromptTokensPerLine is the seed estimate, used only when
	// the calibration above cannot be taken. 38 is what the probe model
	// reads at the current nonce width, measured on the reference host.
	//
	// A wrong value here no longer produces a wrong verdict — the depth
	// readback still refuses to judge a prompt that missed the depth — so
	// this is a starting point, not a calibration.
	hostCutoffPromptTokensPerLine = 38

	// hostCutoffWindowMargin covers the chat template and the residual
	// tokens-per-line estimate error on top of the depth itself.
	hostCutoffWindowMargin = 2048

	// hostCutoffWindowSlots multiplies the window we ask the engine for.
	//
	// num_ctx is not a per-request prompt budget: ollama divides it among
	// its parallel slots and truncates the prompt to what one slot holds.
	// Measured on the reference host, with the daemon's own
	// OLLAMA_NUM_PARALLEL unset (the fresh-install case, where the boot
	// plan is untuned): num_ctx 23048 capped the prompt at 11526 and
	// num_ctx 46096 capped it at 23050 — half, each time. Asking for
	// depth + margin alone therefore measured 55 % of the depth, and
	// silently.
	//
	// 2 covers the split seen in practice; measureHostCutoff reads the
	// depth back and retries wider if some host splits further, so this
	// is the common-case saving rather than the guarantee. Kept as small
	// as that allows for two reasons: the window is real memory (the
	// probe model costs 12 KB/token of KV, so each slot factor is
	// ~270 MB), and a wider window measures slightly SLOWER on a
	// CPU-only host — the reference machine reads 583 tok/s of prefill
	// at this window against the 671 the 45 s threshold was calibrated
	// at, i.e. a ~5 % conservative bias. Both effects point the same way
	// and both are far inside the threshold's margin.
	hostCutoffWindowSlots = 2

	// hostCutoffMaxAttempts bounds the widen-and-retry above. Two: one
	// ordinary measurement, and one correction for a host whose engine
	// splits the window further than hostCutoffWindowSlots expects.
	hostCutoffMaxAttempts = 2

	// hostCutoffCalibrationTimeout bounds the calibration request, and
	// hostCutoffProbeTimeout bounds each measuring request. They are
	// per-request CEILINGS UNDER the budget below, not budgets of their
	// own: measureHostCutoff puts a deadline on the whole run, and
	// context.WithTimeout keeps the earlier of the two, so a request late
	// in the run gets whatever is left rather than a fresh ten minutes.
	//
	// The split exists because the two requests do different amounts of
	// work. Calibration sends hostCutoffCalibrationLines with
	// num_predict:1; a measuring request sends ~21k tokens and decodes 200.
	// Measured on the GitHub-hosted macOS runner (macos-14, 3 vCPU M1 /
	// 7 GB, the permanent hardware of the install+inference leg):
	// calibration 2 min 17 s, one sample 7 min 12 s. Each ceiling is
	// ~1.25x its measured worst case, which is margin against runner
	// contention rather than against a slower machine.
	//
	// A run that exceeds even these is not evidence of a slow host — a
	// wedged engine looks identical — so the timeout still yields
	// "undecided" and the install path carries on unchanged.
	hostCutoffCalibrationTimeout = 3 * time.Minute
	hostCutoffProbeTimeout       = 9 * time.Minute

	// hostCutoffMeasureBudget bounds the whole sampled measurement, not
	// one request. The figure is published, and a published measurement is
	// the median of N samples with their spread rather than one reading
	// (proto/signer/inference_state.go, the memory_bandwidth_measured_gbs
	// doc) — but N samples on the slowest host is exactly where the cost
	// lands, and this runs before the model download rather than instead
	// of it. So: take as many of benchSampleCount as fit, never fewer than
	// one, and publish how many were actually taken.
	//
	// It is 12 minutes, and it GREW from 3 (waired-agent#579). That reads
	// backwards in a diff, so: the 3 was not enforced. measureHostCutoff
	// only consulted it between samples, so the calibration and sample 1
	// were outside it entirely and one request could take
	// hostCutoffProbeTimeout regardless. The number gets bigger because it
	// starts being true.
	//
	// 12 minutes is the partition below, and the partition is what makes
	// it defensible: one calibration at its ceiling plus one full sample at
	// its ceiling. A host sitting at BOTH ceilings still publishes a
	// one-sample measurement instead of timing out with nothing. The old
	// doc justified 3 minutes with "~40 s each" — a figure this repo's own
	// reference host (66.6 s, proto/hostfit/host_cutoff.go) already
	// exceeded by 1.7x and the macOS runner exceeds by 10.8x.
	//
	// Reference host: 4 s + 3 x 66.6 s = 204 s, still three samples.
	// macOS runner: 137 s + 432 s = 569 s, one sample, published.
	hostCutoffMeasureBudget = hostCutoffCalibrationTimeout + hostCutoffProbeTimeout

	// hostCutoffScreenMargin is how far past the budget the prefill-only
	// bound has to reach before the screen may conclude on it.
	//
	// 1.5, i.e. 67.5 s against a 45 s budget, and the size is set by one
	// number: this repo's reference host measures a 66.6 s turn
	// (proto/hostfit/host_cutoff.go) and is a host the cutoff correctly
	// rejects. Putting the line ABOVE it is the point — the screen must
	// not reach a verdict about any host better than one already shown to
	// be unusable, because between the budget and this line the cheap
	// bound and the real measurement can disagree about which side of 45 s
	// a host falls on, and the measurement is the one that decides.
	//
	// Below the line the screen says nothing at all and the full
	// measurement runs, which is what happened before this existed.
	hostCutoffScreenMargin = 1.5

	// hostCutoffScreenMinPromptTokens is the shortest reading the screen
	// will conclude from.
	//
	// The screen prompt is hostCutoffCalibrationLines lines, ~2.8k tokens
	// on the probe model. An engine that truncated it reports a prefill
	// over the tokens it kept, and a short prefill carries a larger share
	// of the request's fixed cost, so its rate UNDER-states the host —
	// which is the one direction that turns a fine host into a verdict.
	// The full measurement has the same hazard and answers it with a depth
	// readback (hostfit.HostProbe.Measured); this is that readback at this
	// depth.
	//
	// 1500 is a little over half the expected count: wide enough not to
	// fire on the tokenizer variation the calibration exists to absorb
	// (35-56 tokens/line across models and nonce widths), narrow enough
	// that a genuine truncation lands under it.
	hostCutoffScreenMinPromptTokens = 1500

	// hostCutoffScreenConfirmTimeout is the per-request ceiling for the
	// screen's SECOND reading, and it is deliberately smaller than the
	// first's hostCutoffCalibrationTimeout: the first reading leaves the
	// probe model resident (hostCutoffScreenKeepAlive), so the confirming
	// request pays a prefill and no model load.
	//
	// Its size comes from what has to FIT rather than from a measurement:
	// hostCutoffCalibrationTimeout + this must sit inside
	// hostspeed.InstallWindow, so that a host whose probe model is already
	// on disk can always reach a verdict inside the window the model
	// download is waiting behind. Pinned by
	// TestHostCutoffScreen_FitsTheInstallWindow.
	//
	// A confirming read that runs past it reaches no verdict and the full
	// measurement runs instead — the direction that costs a download
	// rather than a host. A host too slow to prefill ~2.8k tokens in a
	// minute with the model already resident is bounded below at roughly
	// 456 s per turn; it falls through to a full measurement it will not
	// finish either, and lands on the same "undecided, carry on" arm it
	// landed on before any of this existed.
	hostCutoffScreenConfirmTimeout = 60 * time.Second

	// hostCutoffScreenQuietWait / hostCutoffScreenQuietPoll bound the wait
	// for the host to stop doing something else before the screen reads
	// it.
	//
	// It is a WAIT rather than a check because of where this runs. The
	// call before it is ensureHostCutoffProbeModel, and on a fresh install
	// that pulls ~1 GB of weights; endPull fires a serve reconcile when a model
	// lands, and a reconcile stops and respawns the engine. So the moment
	// the screen is first able to run is very often the moment the engine
	// is being restarted underneath it — and a bare check there would
	// answer "busy" and stand the screen down on precisely the install
	// path it exists for.
	//
	// It is paid only by a host the first reading already put past the
	// line, never by one that cleared it. Waiting in front of the first
	// reading instead would have put this in front of EVERY measurement
	// this daemon takes.
	//
	// 30 s, and the size is what the partition allows rather than a
	// measurement: hostCutoffCalibrationTimeout + this +
	// hostCutoffScreenConfirmTimeout must fit hostspeed.InstallWindow
	// (TestHostCutoffScreen_TheConstantsHoldTogether). Reaching the end of
	// it is not a failure — the first reading has already happened and its
	// line cost still comes back — it only means the screen may not
	// CONCLUDE, which is the fall-through the whole design treats as safe.
	hostCutoffScreenQuietWait = 30 * time.Second
	hostCutoffScreenQuietPoll = 500 * time.Millisecond

	// hostCutoffStraddleSampleCount is how many samples a measurement takes
	// when the first benchSampleCount of them disagree about the budget
	// (hostCutoffSamplesStraddleBudget).
	//
	// Odd, so the median stays a single middle run rather than the
	// slower-of-two tie-break reduceHostCutoffSamples falls back to. Two
	// extra rather than more because this is bounded by the same budget
	// check as every other sample — a host slow enough to straddle a 45 s
	// line is also a host where two more samples is most of what the window
	// has left, and the loop simply stops when they no longer fit.
	//
	// It buys a better median, not certainty. A host whose true turn sits on
	// the line will straddle any number of samples; the point is that the
	// answer stops being decided by a single run.
	hostCutoffStraddleSampleCount = benchSampleCount + 2

	// hostCutoffScreenKeepAlive is how long, in seconds, the first screen
	// reading asks the engine to keep the probe model resident.
	//
	// The requests around it use keep_alive:0 so the loaded probe does not
	// sit on the host least able to spare it. That is 3.41 GB at the 200k
	// window, not the ~1 GB the weights suggest — measured on a 24 GB host
	// (waired-agent#644), where the KV cache is most of it: the same probe
	// loads at 1.47 GB at 24k and 1.14 GB at 4k. This one does not unload,
	// because whatever runs next needs the same model within seconds:
	// either the confirming reading, or the first full sample. Paying the
	// load twice is what makes two readings look expensive.
	//
	// The exposure is bounded to this window and to one path — a screen
	// that neither confirms nor falls through to a sample, i.e. an error
	// between the two. Reading two and the last sample both unload.
	hostCutoffScreenKeepAlive = 120

	// hostCutoffScreenBounceGrace is how many times a screen interrupted
	// by an engine restart THIS AGENT ordered is re-read without charge.
	// Same hazard and the same answer as benchEngineBounceGrace: the
	// engine surfaces a killed connection as a bare EOF, so the generation
	// counter is the only thing that can tell "we restarted it" from "the
	// host cannot answer".
	hostCutoffScreenBounceGrace = 1
)

// hostCutoffMeasurement is a completed sampled measurement: the median
// sample, and the provenance that travels with it on the wire.
type hostCutoffMeasurement struct {
	// Probe is the MEDIAN SAMPLE — one run's own three numbers, not a
	// field-wise median of several. A field-wise median would put a
	// prefill/decode pair on the wire that no run ever produced, and the
	// turn time computed from it would belong to no measurement.
	Probe hostfit.HostProbe

	// Samples is how many usable samples the median was taken over, and
	// SpreadPct is (max−min)/median over their turn times. Samples is 0
	// when nothing usable was measured.
	Samples   int
	SpreadPct float64

	// Method is which of the two shapes this is — one of the
	// signer.BenchmarkMethod* constants — and it is what tells them apart
	// downstream. Empty when nothing was measured.
	//
	//	BenchmarkMethodOllamaEval          Probe is a full-depth sample and
	//	                                   Probe.Measured() is true.
	//	BenchmarkMethodOllamaPrefillFloor  Probe carries the screen's ~2.8k
	//	                                   reading, Probe.Measured() is
	//	                                   FALSE by construction, and the
	//	                                   verdict lives in TurnFloorSeconds.
	Method string

	// TurnFloorSeconds is hostfit.TurnFloorSeconds of Probe's prefill
	// rate: a lower bound on the turn. Set under BOTH methods — under
	// ollama_eval it is a derived sanity figure beside the measurement,
	// and under ollama_prefill_floor it is the only figure there is.
	TurnFloorSeconds float64
}

// hostCutoffScreen is one cheap reading of this host: what a filler line
// costs in tokens, and the prefill rate the engine reported while finding
// that out.
//
// The line cost is what the reading was originally taken for
// (hostCutoffCalibrationLines); the rate is what waired-agent#579 Stage 3
// added, and it was already being computed and thrown away.
type hostCutoffScreen struct {
	// TokensPerLine is what depthBenchPrompt needs to build a prompt that
	// lands on HostCutoffProbeDepthTokens.
	TokensPerLine int

	// PromptTokens is what the engine says it actually prefilled and
	// PrefillTokps the rate it did it at. Far short of
	// HostCutoffProbeDepthTokens by construction — that is the saving.
	PromptTokens int
	PrefillTokps float64
}

// hostCutoffScreenOutcome is what one pass of the screen reached.
type hostCutoffScreenOutcome int

const (
	// screenFellThrough: no verdict. Either the reading could not be taken
	// at all, or this host is not far enough below the cutoff to conclude
	// about without measuring it. The full-depth measurement runs, which
	// is what happened before the screen existed.
	screenFellThrough hostCutoffScreenOutcome = iota

	// screenBelowBudget: two readings, taken under one engine process on a
	// host with nothing else running, agree the bound is already past
	// HostCutoffTurnBudgetSeconds * hostCutoffScreenMargin. This IS the
	// measurement; no sample is taken.
	screenBelowBudget

	// screenInterrupted: the engine's process generation moved under the
	// pass, so nothing was learned about the host. Re-read without charge.
	screenInterrupted
)

// ollamaGenCounters are the timing counters ollama's /api/generate
// returns for a non-streaming request. Durations are nanoseconds.
type ollamaGenCounters struct {
	PromptEvalCount    int   `json:"prompt_eval_count"`
	PromptEvalDuration int64 `json:"prompt_eval_duration"`
	EvalCount          int   `json:"eval_count"`
	EvalDuration       int64 `json:"eval_duration"`
}

// rates converts the counters to (prefill, decode) tok/s. The error names
// the missing counter rather than returning zeros, because a build or
// backend that stops reporting them must be diagnosable: a silent 0 tok/s
// reads as an infinitely slow host.
func (c ollamaGenCounters) rates() (prefill, decode float64, err error) {
	if c.PromptEvalDuration <= 0 || c.EvalDuration <= 0 || c.EvalCount <= 0 {
		return 0, 0, fmt.Errorf("engine returned no timing counters (prompt_eval_duration=%d eval_duration=%d eval_count=%d)",
			c.PromptEvalDuration, c.EvalDuration, c.EvalCount)
	}
	return float64(c.PromptEvalCount) / (float64(c.PromptEvalDuration) / 1e9),
		float64(c.EvalCount) / (float64(c.EvalDuration) / 1e9),
		nil
}

// postOllamaGenerate issues one non-streaming /api/generate and returns
// its timing counters. Shared by the depth benchmark (#624) and the host
// cutoff probe (#496) so there is one place that knows the response shape.
func postOllamaGenerate(ctx context.Context, client *http.Client, baseURL string, payload map[string]any) (ollamaGenCounters, error) {
	var counters ollamaGenCounters
	body, err := json.Marshal(payload)
	if err != nil {
		return counters, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/api/generate", bytes.NewReader(body))
	if err != nil {
		return counters, err
	}
	req.Header.Set("Content-Type", "application/json")
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return counters, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return counters, fmt.Errorf("engine returned %d: %s", resp.StatusCode, bytes.TrimSpace(raw))
	}
	if err := json.NewDecoder(resp.Body).Decode(&counters); err != nil {
		return counters, err
	}
	return counters, nil
}

// hostCutoffProbeDeps is the injectable world of measureHostCutoff.
type hostCutoffDeps struct {
	// BaseURL is the engine's loopback root ("http://127.0.0.1:11434").
	BaseURL string
	// EngineModel is the ollama tag of hostfit.HostCutoffProbeModelID —
	// the engine-native name, not the catalog id.
	EngineModel string

	HTTPClient *http.Client
	Logger     *slog.Logger

	// Nonce leads the prompt so no two runs share a prefix. Without it
	// ollama's prefix KV cache answers a repeat with the FULL
	// prompt_eval_count and a near-zero duration — measured at 697,222
	// tok/s on this repo's reference host, which would pass every machine
	// ever built.
	Nonce string

	// MeasureBudget bounds the whole sampled measurement. 0 means
	// hostCutoffMeasureBudget. Injected rather than read from the constant
	// so the early-exit can be tested without a test that waits minutes.
	MeasureBudget time.Duration

	// CalibrationTimeout is the per-request ceiling for the calibration
	// request; 0 means hostCutoffCalibrationTimeout. Injected for the same
	// reason as MeasureBudget, and separately from it because the defect
	// this bounds is precisely a calibration that consumed the budget
	// before any sample ran (waired-agent#579).
	CalibrationTimeout time.Duration

	// EngineQuiet, when non-nil, reports whether anything else on this
	// host is about to take the engine away — a download in flight, or a
	// serve-env reconcile queued behind one. The same predicate BenchDeps
	// carries (#582/#601), consulted here for a narrower purpose: only the
	// SCREEN asks it, and only as a precondition for concluding.
	//
	// Two readings cannot substitute for it. They are taken seconds apart,
	// so sustained contention depresses both and they agree — the pair
	// guards against a transient stall, and this guards against the host
	// being busy at all. A busy host reads as a slow one, and this is the
	// one place a reading turns into a verdict without a measurement
	// behind it.
	//
	// nil means "assume quiet", so every existing caller and test keeps
	// today's behaviour.
	EngineQuiet func(context.Context) bool

	// QuietWait bounds how long the screen waits for EngineQuiet before
	// giving up on concluding; 0 means hostCutoffScreenQuietWait.
	// Injected for the same reason MeasureBudget is — a test for the
	// give-up arm should not take half a minute.
	QuietWait time.Duration

	// EngineGen, when non-nil, is the engine's process generation
	// (agentInferenceProvider.engineProcessGen). Sampled before the screen
	// and re-read after it: a pair of readings whose engine restarted
	// between them is not a statement about the host's speed, and a
	// restart THIS AGENT ordered surfaces only as a bare EOF, so there is
	// no error text to key on (#359, #582).
	//
	// nil returns a constant 0, so the generation never appears to move.
	EngineGen func() uint64
}

// engineGen is a nil-safe EngineGen call, matching BenchDeps.engineGen: a
// caller that does not wire one sees a generation that never moves, which
// makes every failure a real one.
func (d hostCutoffDeps) engineGen() uint64 {
	if d.EngineGen == nil {
		return 0
	}
	return d.EngineGen()
}

// engineQuiet is a nil-safe EngineQuiet call. Absent means quiet, so the
// screen behaves for an unwired caller exactly as it does on an idle host.
func (d hostCutoffDeps) engineQuiet(ctx context.Context) bool {
	if d.EngineQuiet == nil {
		return true
	}
	return d.EngineQuiet(ctx)
}

// measureHostCutoff takes the measurement the cutoff judges and the wire
// publishes: up to benchSampleCount runs of a ~21k-token prefill plus a
// 200-token decode on the probe model, reported by the engine's own
// counters, reduced to the median run.
//
// Sampling is what makes the number safe to act on. The repeat that fixed
// the 45 s threshold found idle runs within ±2 % of each other and a
// single run that shared the machine with another job at +21 % — enough,
// on its own, to move a host across the threshold. The median of three
// throws that run away; one reading cannot.
//
// The returned measurement is only meaningful when its Probe reports
// Measured(); an error always yields a zero measurement, and the caller
// must treat that as "no verdict" rather than as a slow host. A sample
// that fails outright ends the run: a host whose engine has stopped
// answering is not a host to publish a median for.
func measureHostCutoff(ctx context.Context, deps hostCutoffDeps) (hostCutoffMeasurement, error) {
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	if deps.BaseURL == "" || deps.EngineModel == "" {
		return hostCutoffMeasurement{}, fmt.Errorf("host cutoff probe: no engine to measure")
	}
	budget := deps.MeasureBudget
	if budget <= 0 {
		budget = hostCutoffMeasureBudget
	}
	deps.Logger.Info("measuring whether this host can serve local inference usefully; this takes a minute or two",
		"model", deps.EngineModel, "depth_tokens", hostfit.HostCutoffProbeDepthTokens,
		"samples", benchSampleCount, "budget", budget)

	started := time.Now()

	// The budget becomes a real deadline over EVERYTHING below, not just a
	// figure consulted between samples (waired-agent#579).
	//
	// It used to be checked only at `sample > 1`, which left the
	// calibration and sample 1 outside it: each could run for
	// hostCutoffProbeTimeout, and measureHostCutoffSample retries, so one
	// measurement could occupy ~50 minutes of a budget that said 3. On the
	// macOS runner it took 9 min 29 s, and the bundled model download that
	// waits behind this call was dispatched one second after it ended.
	//
	// context.WithTimeout keeps the earlier of parent and child, so the
	// per-request ceilings below become min(ceiling, remaining) for free —
	// no signature changes, and no request can outlive the run.
	ctx, cancel := context.WithDeadline(ctx, started.Add(budget))
	defer cancel()

	// The screen comes first, and it is the same request the calibration
	// always was — the line cost is read out of it either way. What is new
	// is that its prefill rate is read too, and on a host far enough below
	// the cutoff that ends the run here (waired-agent#579 Stage 3).
	//
	// Ending here is the whole point. A full sample is minutes on exactly
	// the hosts this is looking for — 7 min 12 s on the GitHub macos-14
	// runner — and those minutes stand in front of the model download,
	// which is how a host that should have downloaded nothing spent the
	// operator's first run deciding nothing.
	screened, tokensPerLine, decided := screenHostCutoff(ctx, deps)
	if decided {
		deps.Logger.Warn("host cutoff: this host is below the recommended spec on its prefill rate alone; "+
			"not measuring at full depth",
			"turn_floor_seconds", fmt.Sprintf("%.1f", screened.TurnFloorSeconds),
			"budget_seconds", fmt.Sprintf("%.0f", hostfit.HostCutoffTurnBudgetSeconds),
			"margin", hostCutoffScreenMargin,
			"elapsed", time.Since(started).Round(time.Second))
		return screened, nil
	}
	if tokensPerLine <= 0 {
		tokensPerLine = hostCutoffPromptTokensPerLine
	}

	var (
		usable  []hostfit.HostProbe
		slowest time.Duration
		last    hostfit.HostProbe
	)
	// Grows once, and only when the samples taken so far disagree about the
	// verdict — see hostCutoffSamplesStraddleBudget.
	maxSamples := benchSampleCount
	for sample := 1; sample <= maxSamples; sample++ {
		// Stop before starting a sample the budget cannot hold. The
		// slowest sample so far is the estimate: samples on one idle host
		// vary by a few percent, and the one case where they do not — a
		// machine that just got busy — is the case worth cutting short.
		if sample > 1 && time.Since(started)+slowest > budget {
			deps.Logger.Info("host cutoff: stopping at the sample budget",
				"samples", len(usable), "elapsed", time.Since(started).Round(time.Second))
			break
		}
		at := time.Now()
		probe, err := measureHostCutoffSample(ctx, deps, sample, tokensPerLine)
		if err != nil {
			if len(usable) == 0 {
				return hostCutoffMeasurement{}, err
			}
			// Some samples landed. Judging on them beats discarding a
			// measurement because the host got busy near the end.
			deps.Logger.Info("host cutoff: a sample did not complete; judging on the ones that did",
				"samples", len(usable), "err", err)
			break
		}
		if took := time.Since(at); took > slowest {
			slowest = took
		}
		last = probe
		if probe.Measured() {
			usable = append(usable, probe)
		}
		if sample == benchSampleCount && maxSamples == benchSampleCount &&
			hostCutoffSamplesStraddleBudget(usable) {
			maxSamples = hostCutoffStraddleSampleCount
			deps.Logger.Info("host cutoff: the samples disagree about the budget; taking more before deciding",
				"samples", len(usable),
				"spread_pct", fmt.Sprintf("%.1f", reduceHostCutoffSamples(usable).SpreadPct),
				"budget_seconds", fmt.Sprintf("%.0f", hostfit.HostCutoffTurnBudgetSeconds),
				"max_samples", maxSamples)
		}
	}
	if len(usable) == 0 {
		// Nothing usable, but the last probe still carries the depth the
		// engine reported — which is how the caller says "it prefilled
		// 11k of the 21k asked for" rather than just "no verdict".
		return hostCutoffMeasurement{Probe: last}, nil
	}
	return reduceHostCutoffSamples(usable), nil
}

// screenHostCutoff takes the cheap prefill-only reading and reaches a
// verdict from it when it can.
//
// tokensPerLine comes back whether or not it concludes, because the full
// measurement needs it either way and there is no second request to get
// it from: this IS the calibration.
//
// decided=true means the returned measurement is the answer and no sample
// should be taken. decided=false means the screen said nothing, which is
// the ordinary outcome on any host worth serving from.
func screenHostCutoff(ctx context.Context, deps hostCutoffDeps) (hostCutoffMeasurement, int, bool) {
	tokensPerLine := 0
	for grace := hostCutoffScreenBounceGrace; ; grace-- {
		m, perLine, outcome := screenHostCutoffOnce(ctx, deps)
		if perLine > 0 {
			tokensPerLine = perLine
		}
		if outcome == screenBelowBudget {
			return m, tokensPerLine, true
		}
		if outcome == screenFellThrough || grace <= 0 {
			return hostCutoffMeasurement{}, tokensPerLine, false
		}
		deps.Logger.Info("host cutoff: the engine restarted under the screen; "+
			"reading again without charging the attempt", "grace_left", grace-1)
	}
}

// screenHostCutoffOnce is one pass: read, and if the reading is far
// enough below the cutoff, read again and see whether it holds.
//
// The second reading is paid ONLY by hosts that are about to be cut. A
// host that clears the screen costs exactly what the calibration always
// cost — one request — so this adds nothing to the majority path.
func screenHostCutoffOnce(ctx context.Context, deps hostCutoffDeps) (hostCutoffMeasurement, int, hostCutoffScreenOutcome) {
	gen := deps.engineGen()

	first, err := screenHostCutoffPrompt(ctx, deps, 1)
	if err != nil {
		if deps.engineGen() != gen {
			return hostCutoffMeasurement{}, 0, screenInterrupted
		}
		deps.Logger.Info("host cutoff: could not measure what a prompt line costs; using the seed estimate",
			"tokens_per_line", hostCutoffPromptTokensPerLine, "err", err)
		return hostCutoffMeasurement{}, 0, screenFellThrough
	}
	firstFloor, fires := hostCutoffScreenFloor(first)
	deps.Logger.Info("host cutoff: measured the prompt's token cost",
		"tokens_per_line", first.TokensPerLine, "seed", hostCutoffPromptTokensPerLine,
		"prompt_tokens", first.PromptTokens,
		"prefill_tok_s", fmt.Sprintf("%.0f", first.PrefillTokps),
		"turn_floor_seconds", fmt.Sprintf("%.1f", firstFloor))
	if !fires {
		return hostCutoffMeasurement{}, first.TokensPerLine, screenFellThrough
	}
	// Only now is it worth waiting for the host to go idle — this one is
	// about to be cut, and everything above is what every host pays.
	//
	// Waiting BEFORE the first reading was the obvious arrangement and it
	// was wrong twice over: it put up to hostCutoffScreenQuietWait in
	// front of every measurement on any host whose engine never answers
	// quiet, and it gave up the one thing the pair is good for. Taken this
	// way the two readings straddle the settling — the first may be
	// contended, the second is not — and BOTH have to clear the line. A
	// first reading depressed by something else on the machine is
	// therefore not enough on its own.
	if !awaitScreenQuiet(ctx, deps) {
		deps.Logger.Info("host cutoff: the screen read a host below the recommended spec, but something else "+
			"was using the engine; measuring at full depth instead",
			"turn_floor_seconds", fmt.Sprintf("%.1f", firstFloor))
		return hostCutoffMeasurement{}, first.TokensPerLine, screenFellThrough
	}

	second, err := screenHostCutoffPrompt(ctx, deps, 2)
	if err != nil {
		if deps.engineGen() != gen {
			return hostCutoffMeasurement{}, first.TokensPerLine, screenInterrupted
		}
		deps.Logger.Info("host cutoff: the confirming reading did not complete; measuring at full depth",
			"err", err)
		return hostCutoffMeasurement{}, first.TokensPerLine, screenFellThrough
	}
	if deps.engineGen() != gen {
		return hostCutoffMeasurement{}, first.TokensPerLine, screenInterrupted
	}
	// Busy at the END is not the same as a restart, and does not earn a
	// re-read: the host started doing something else during the screen, so
	// the readings describe a machine under load rather than this machine.
	// Retrying would only wait out the quiet window again and cost the
	// install path the time it is here to save.
	if !deps.engineQuiet(ctx) {
		deps.Logger.Info("host cutoff: something started using the engine during the screen; " +
			"measuring at full depth instead")
		return hostCutoffMeasurement{}, first.TokensPerLine, screenFellThrough
	}
	secondFloor, secondFires := hostCutoffScreenFloor(second)
	if !secondFires {
		// One reading is not a verdict. A wedged or briefly starved engine
		// produces exactly the shape this is looking for, and it produces
		// it once.
		deps.Logger.Info("host cutoff: the screen did not repeat; measuring at full depth",
			"turn_floor_seconds", fmt.Sprintf("%.1f", firstFloor),
			"confirming_turn_floor_seconds", fmt.Sprintf("%.1f", secondFloor))
		return hostCutoffMeasurement{}, first.TokensPerLine, screenFellThrough
	}
	return reduceHostCutoffScreen(
		[2]hostCutoffScreen{first, second},
		[2]float64{firstFloor, secondFloor},
	), first.TokensPerLine, screenBelowBudget
}

// awaitScreenQuiet blocks until nothing else on this host is using the
// engine, and reports whether it got there.
//
// awaitQuietEngine one level down and on a much shorter leash: that one
// runs on the boot goroutine with nothing behind it and can wait an hour,
// and this one runs with a model download behind it and can wait
// hostCutoffScreenQuietWait. Not shared, because the two waits are the
// same shape for different reasons and a single knob could only be right
// for one of them.
func awaitScreenQuiet(ctx context.Context, deps hostCutoffDeps) bool {
	if deps.EngineQuiet == nil {
		return true
	}
	wait := deps.QuietWait
	if wait <= 0 {
		wait = hostCutoffScreenQuietWait
	}
	deadline := time.Now().Add(wait)
	for {
		if deps.EngineQuiet(ctx) {
			return true
		}
		if time.Now().After(deadline) {
			deps.Logger.Info("host cutoff: the engine did not go quiet; the screen will read the "+
				"line cost but will not conclude from it", "waited", wait)
			return false
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(min(hostCutoffScreenQuietPoll, wait)):
		}
	}
}

// hostCutoffScreenFloor is the bound one reading puts under this host's
// turn, and whether it is a bound the screen may conclude from.
func hostCutoffScreenFloor(s hostCutoffScreen) (float64, bool) {
	floor := hostfit.TurnFloorSeconds(s.PrefillTokps)
	if floor <= 0 || s.PromptTokens < hostCutoffScreenMinPromptTokens {
		return floor, false
	}
	return floor, floor > hostfit.HostCutoffTurnBudgetSeconds*hostCutoffScreenMargin
}

// reduceHostCutoffScreen turns two agreeing readings into the
// measurement the wire publishes.
//
// It keeps the SMALLER of the two bounds, which is the opposite bias from
// reduceHostCutoffSamples, and deliberately so. That one takes the slower
// of two middle samples because it is guarding against LETTING a host
// through; this one is used to CUT a host, so the same caution points the
// other way. A stall inside a reading can only make it look slower, so
// the smaller bound is the one less likely to owe its size to something
// other than the host — and both readings already cleared the margin, so
// keeping the smaller one cannot change the verdict.
func reduceHostCutoffScreen(readings [2]hostCutoffScreen, floors [2]float64) hostCutoffMeasurement {
	pick := 0
	if floors[1] < floors[0] {
		pick = 1
	}
	return hostCutoffMeasurement{
		Probe: hostfit.HostProbe{
			PromptTokens: readings[pick].PromptTokens,
			PrefillTokps: readings[pick].PrefillTokps,
			// DecodeTokps stays zero: nothing was decoded. That makes
			// Probe.Measured() false, which is the correct answer for a
			// probe taken far short of the canonical depth — a consumer
			// that rebuilds a HostProbe from the wire declines to judge
			// rather than reading a bound as a measurement. The verdict
			// travels beside it, in TurnFloorSeconds and Method.
		},
		Samples:          len(floors),
		SpreadPct:        spreadPercent(floors[:]),
		TurnFloorSeconds: floors[pick],
		Method:           signer.BenchmarkMethodOllamaPrefillFloor,
	}
}

// hostCutoffSamplesStraddleBudget reports whether the samples taken so far
// disagree about the only question this measurement is asked: is one coding
// turn on this host inside HostCutoffTurnBudgetSeconds.
//
// This is the case waired-agent#622 recorded, where the spread was computed,
// published, and read by nothing: three samples that disagreed by 106% of
// their own median still produced a verdict, and which side the median landed
// on was decided by which run happened to be the middle one. On the host that
// found it the verdict was not in doubt (4.45 s against a 45 s budget, every
// sample clearing by an order of magnitude), and that is exactly why the
// straddle is the test rather than the spread: a large spread far from the
// line changes no answer and should cost nothing.
//
// It reads the samples rather than SpreadPct because the samples say it
// exactly. A spread threshold would need a band estimate around the median to
// ask the same question, and would be wrong at both ends of it.
func hostCutoffSamplesStraddleBudget(samples []hostfit.HostProbe) bool {
	var below, above bool
	for _, p := range samples {
		if !p.Measured() {
			continue
		}
		if p.TurnSeconds() <= hostfit.HostCutoffTurnBudgetSeconds {
			below = true
		} else {
			above = true
		}
	}
	return below && above
}

// reduceHostCutoffSamples picks the median sample by turn time and
// records the spread across all of them.
//
// On an even count it takes the SLOWER of the two middle samples. The
// threshold is deliberately strict — a wrongly-cut host pays one
// `waired inference on`, a wrongly-passed one downloads 20-45 GB first —
// and this keeps that bias rather than reversing it in the one case where
// there is no single middle run.
func reduceHostCutoffSamples(samples []hostfit.HostProbe) hostCutoffMeasurement {
	sorted := append([]hostfit.HostProbe(nil), samples...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].TurnSeconds() < sorted[j].TurnSeconds() })
	turns := make([]float64, 0, len(sorted))
	for _, p := range sorted {
		turns = append(turns, p.TurnSeconds())
	}
	median := sorted[len(sorted)/2]
	return hostCutoffMeasurement{
		Probe:     median,
		Samples:   len(sorted),
		SpreadPct: spreadPercent(turns),
		Method:    signer.BenchmarkMethodOllamaEval,
		// Derived, not measured separately, and carried so the two shapes
		// of measurement put the same figure in the same field. Under this
		// method it is a sanity figure beside the turn — a consumer can
		// check floor <= turn — and under the screen's method it is the
		// only figure there is.
		TurnFloorSeconds: hostfit.TurnFloorSeconds(median.PrefillTokps),
	}
}

// hostCutoffNonce is the per-request nonce. It leads the prompt, so the
// varying part goes FIRST: two prompts that differ only after a shared
// opening are answered from ollama's prefix KV cache.
//
// Fixed width by construction, because the nonce repeats on every filler
// line and its width is therefore part of what a line costs. A nonce that
// grew with the sample number would make every sample a slightly
// different depth.
func hostCutoffNonce(base string, sample, attempt int) string {
	return fmt.Sprintf("%02d%02d-%s", sample, attempt, base)
}

// screenHostCutoffPrompt sends the short prompt and reads back BOTH
// things the response carries: what one filler line costs on this model,
// and the rate the engine prefilled it at.
//
// It was calibrateHostCutoffPrompt, and the line cost is still its first
// job — depthBenchPrompt cannot build a prompt that lands on the
// canonical depth without it. The prefill rate was always in the same
// response and was always discarded; reading it out is what lets a host
// far below the cutoff be found without paying for a full-depth sample
// (waired-agent#579).
//
// num_predict:1 because only the prompt side is wanted; the answer is
// thrown away. The count includes the prompt's fixed opening and closing
// lines, so it slightly OVERSTATES the per-line cost and the real prompt
// lands a percent or two short of the depth — immaterial next to the
// ±30 % the depth guard allows, and TurnSeconds normalises to the
// canonical depth regardless.
//
// reading is 1 for the first and 2 for the confirming one. It picks the
// nonce, so the two are not the same prompt: two requests sharing a
// prefix are answered from the engine's KV cache at a rate no host can
// achieve. It also picks the ceiling and the residency — see
// hostCutoffScreenConfirmTimeout and hostCutoffScreenKeepAlive.
func screenHostCutoffPrompt(ctx context.Context, deps hostCutoffDeps, reading int) (hostCutoffScreen, error) {
	// Its own ceiling, not the measuring request's: this sends 50 filler
	// lines with num_predict:1, and giving it the same ten minutes a
	// 21k-token prefill gets is what let it consume a whole measurement
	// before any sample ran (waired-agent#579). Still bounded by the run's
	// deadline above, which WithTimeout keeps when it is the earlier one.
	timeout := deps.CalibrationTimeout
	if timeout <= 0 {
		timeout = hostCutoffCalibrationTimeout
	}
	keepAlive := hostCutoffScreenKeepAlive
	if reading > 1 {
		// The confirming reading is the last request the screen makes, so
		// it unloads; and it pays no load itself, so it gets the smaller
		// ceiling.
		keepAlive = 0
		if timeout > hostCutoffScreenConfirmTimeout {
			timeout = hostCutoffScreenConfirmTimeout
		}
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	counters, err := postOllamaGenerate(ctx, deps.HTTPClient, deps.BaseURL, map[string]any{
		"model":      deps.EngineModel,
		"prompt":     depthBenchPromptLines(hostCutoffCalibrationLines, hostCutoffNonce(deps.Nonce, 0, reading-1)),
		"stream":     false,
		"keep_alive": keepAlive,
		"options": map[string]any{
			"num_predict": 1,
			"temperature": 0,
			"num_ctx":     hostCutoffWindowSlots*hostfit.HostCutoffProbeDepthTokens + hostCutoffWindowMargin,
		},
	})
	if err != nil {
		return hostCutoffScreen{}, err
	}
	if counters.PromptEvalCount <= 0 {
		return hostCutoffScreen{}, fmt.Errorf("engine reported no prompt_eval_count for the calibration prompt")
	}
	perLine := counters.PromptEvalCount / hostCutoffCalibrationLines
	if perLine <= 0 {
		return hostCutoffScreen{}, fmt.Errorf("calibration prompt measured %d tokens over %d lines",
			counters.PromptEvalCount, hostCutoffCalibrationLines)
	}
	screen := hostCutoffScreen{TokensPerLine: perLine, PromptTokens: counters.PromptEvalCount}
	if counters.PromptEvalDuration > 0 {
		// Not counters.rates(): that one insists on the decode side too,
		// and num_predict:1 is deliberately not a decode measurement. A
		// missing prefill duration leaves the rate at zero, which
		// hostCutoffScreenFloor reads as "no bound" rather than as an
		// infinitely slow host.
		screen.PrefillTokps = float64(counters.PromptEvalCount) / (float64(counters.PromptEvalDuration) / 1e9)
	}
	return screen, nil
}

// measureHostCutoffSample takes ONE sample, widening the serve window and
// re-measuring when the engine silently truncated the prompt.
//
// keep_alive:0 unloads the probe model as soon as the response is
// written. The counters are unaffected (they cover prefill and decode,
// not load), and the alternative is leaving ~1 GB plus a 23k KV cache
// resident on the host least able to spare it, right as the real model
// starts downloading.
func measureHostCutoffSample(ctx context.Context, deps hostCutoffDeps, sample, tokensPerLine int) (hostfit.HostProbe, error) {
	window := hostCutoffWindowSlots*hostfit.HostCutoffProbeDepthTokens + hostCutoffWindowMargin
	var probe hostfit.HostProbe
	for attempt := 1; attempt <= hostCutoffMaxAttempts; attempt++ {
		var err error
		probe, err = measureHostCutoffOnce(ctx, deps, window, tokensPerLine,
			hostCutoffNonce(deps.Nonce, sample, attempt))
		if err != nil {
			return hostfit.HostProbe{}, err
		}
		if probe.Measured() {
			return probe, nil
		}
		// Not a usable measurement. The only shape worth another request
		// is a SHORT one — the engine capped the prompt at what one of
		// its slots holds — and the cap tells us exactly how much wider
		// to ask. Anything else (a prompt somehow too long) is a
		// different fault and retrying it would only cost time.
		//
		// A prompt that was short because it was BUILT short cannot
		// reach here any more: the line cost is measured before the
		// samples, not assumed.
		if probe.PromptTokens <= 0 || probe.PromptTokens >= hostfit.HostCutoffProbeDepthTokens {
			return probe, nil
		}
		window = window * hostfit.HostCutoffProbeDepthTokens / probe.PromptTokens
		window += hostCutoffWindowMargin
		deps.Logger.Info("host cutoff: the engine capped the prompt; measuring again with a wider window",
			"prompt_tokens", probe.PromptTokens, "want_tokens", hostfit.HostCutoffProbeDepthTokens,
			"num_ctx", window)
	}
	return probe, nil
}

// measureHostCutoffOnce is one /api/generate at the given serve window.
func measureHostCutoffOnce(ctx context.Context, deps hostCutoffDeps, window, tokensPerLine int, nonce string) (hostfit.HostProbe, error) {
	ctx, cancel := context.WithTimeout(ctx, hostCutoffProbeTimeout)
	defer cancel()

	counters, err := postOllamaGenerate(ctx, deps.HTTPClient, deps.BaseURL, map[string]any{
		"model":  deps.EngineModel,
		"prompt": depthBenchPrompt(hostfit.HostCutoffProbeDepthTokens, tokensPerLine, nonce),
		"stream": false,
		// Seconds, and 0 means "unload now" — not the duration string
		// form, which the API also accepts.
		"keep_alive": 0,
		"options": map[string]any{
			"num_predict": hostfit.HostCutoffCompletionSampleTokens,
			"temperature": 0,
			"num_ctx":     window,
		},
	})
	if err != nil {
		return hostfit.HostProbe{}, err
	}
	prefill, decode, err := counters.rates()
	if err != nil {
		return hostfit.HostProbe{}, err
	}
	return hostfit.HostProbe{
		PromptTokens: counters.PromptEvalCount,
		PrefillTokps: prefill,
		DecodeTokps:  decode,
	}, nil
}

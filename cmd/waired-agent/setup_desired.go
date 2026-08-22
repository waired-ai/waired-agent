package main

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"log/slog"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/waired-ai/waired-agent/internal/agentconfig"
	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/controlclient"
	"github.com/waired-ai/waired-agent/internal/management"
	"github.com/waired-ai/waired-agent/internal/runtime/state"
	"github.com/waired-ai/waired-agent/proto/hostfit"
	"github.com/waired-ai/waired-agent/proto/signer"
)

// setupPushInterval paces the setup-progress reporter. The CP accepts
// 1 push / 2 s (burst 10, waired#835 §20.5); the reporter additionally
// dedupes identical snapshots, so steady state pushes nothing at all.
const setupPushInterval = 2 * time.Second

// setupKeepaliveInterval is how long an UNCHANGED snapshot may go
// unpushed while a setup is live (#130).
//
// Content dedup alone froze last_check for the length of a step with no
// moving field — a 15-minute ollama install, a 45-minute vLLM build, a
// running benchmark — and the wizard's staleness window (120 s) then
// called a perfectly healthy install offline and told the operator to
// intervene. Re-pushing the same content every 30 s advances last_check
// with four times the margin that window needs, and costs 2 pushes /
// minute on a host that is mid-setup — a state in which the fleet is not
// at rest anyway. A host with no onboarding activity still snapshots nil
// and pushes nothing at all.
const setupKeepaliveInterval = 30 * time.Second

// Onboarding step IDs (waired#835 §7, five ids as of waired#934; a sixth
// for #597, two more for waired#1143). The CP treats them as opaque
// strings; NAVI's wizard keys its step rows off them. The three the
// elevated executor drives are named in internal/management, because the
// executor and the daemon have to agree on which row a report belongs to.
const (
	setupStepEngineDownload = management.SetupStepEngineDownload
	setupStepEngineInstall  = management.SetupStepEngineInstall
	// The install-time measurement, in two rows (waired#1143): the small
	// model this host is timed on, and the timing itself. Both are the
	// DAEMON's work — neither crosses the executor API, so neither belongs
	// in internal/management's vocabulary.
	//
	// Not folded into model_pull: that row is the operator's own choice,
	// and the measurement runs before there is one (waired#1099).
	setupStepProbeModelPull = "probe_model_pull"
	setupStepHostSpeed      = "host_speed"
	setupStepModelPull      = "model_pull"
	setupStepBenchmark      = "benchmark"
	setupStepIntegration    = management.SetupStepIntegration
	// setupStepInferenceOff is the echo of the acted-on "don't run local
	// AI on this computer" answer (#597; waired#1109) — the row the CP's
	// completion derivation reads to count an off-host as COMPLETE with
	// no engine or model rows at all. An older NAVI renders the raw id,
	// the documented degradation for unknown step ids.
	setupStepInferenceOff = "inference_off"
)

// Executor lease timings (waired#835 §9/§11). Both sides of the range
// hurt, which is why these are named rather than inline:
//   - too SHORT and a legitimate 15-minute elevated engine install
//     (installOllama's ctx budget on all three OSes) trips a spurious
//     executor_gone while it is still working — but note the executor
//     heartbeats throughout, so only a stall that long matters;
//   - too LONG and the wizard keeps claiming the install is in progress
//     after the operator has already pressed Ctrl-C, which is exactly
//     the never-resolving spinner §9-4 exists to forbid.
//
// 45 s tolerates four missed 10 s heartbeats before declaring the
// executor gone.
const (
	setupExecutorTTL       = 45 * time.Second
	setupExecutorHeartbeat = 10 * time.Second
)

// setupDesiredFreshWindow is how long a watched desired-state write keeps
// counting as "a wizard is driving this host" (#308).
//
// It matches the control plane's own setup-ticket TTL, which is what
// bounds a wizard page's claim on a device there: past it, the page is
// abandoned as far as the CP is concerned, and a `waired init` started
// afterwards must be free to drive from the terminal rather than wait out
// an eight-hour residency for a browser nobody is looking at.
const setupDesiredFreshWindow = 60 * time.Minute

// setupDesired is the (engine, model, benchmark-gen) triple the CP
// serves on the device's own Self map entry (waired#835 §6). The zero
// value means "no instruction" — the common case for every host that
// never ran a NAVI setup.
type setupDesired struct {
	engine       string
	modelID      string
	benchmarkGen int
	// modelGen is the retry generation for the model download (#136).
	// Same contract as benchmarkGen — declarative, idempotent, and a
	// bump is the operator saying "try that download again".
	modelGen int
	// inference is the operator's explicit local-AI answer (#597):
	// signer.DesiredInferenceOn / DesiredInferenceOff, "" = no
	// instruction. Applied once per persisted VALUE, never per frame —
	// see applyDesiredInference.
	inference string
	// integrations is the coding-agent instruction (waired#935), flattened
	// so this struct stays comparable — change detection here is a plain
	// `!=`, and a slice field would not compile.
	//
	// Three states, and the difference between the last two is the whole
	// point: absent = never asked, integrationsNone = asked and every
	// toggle was off, a list = write these. Collapsing the middle case
	// into "no instruction" is how the wizard would report success for a
	// device it never configured — the waired#904 class.
	integrations string
}

// integrationsNone is the flattened form of "asked, and nothing was
// selected". Not a valid target id, so it cannot collide with one.
const integrationsNone = "\x00none"

// flattenIntegrations renders the desired-integrations instruction as a
// comparable string: "" when there is no instruction at all, the
// sentinel when the instruction is an empty set, and the sorted,
// de-duplicated, validated ids joined otherwise.
//
// Sorting matters: the wire order is the control plane's, and a reorder
// with the same contents is not a change the agent should react to.
func flattenIntegrations(d *signer.DesiredIntegrations) string {
	if d == nil {
		return ""
	}
	seen := map[string]bool{}
	var out []string
	for _, t := range d.Enabled {
		// Unknown targets are ignored rather than rejected: the set can
		// grow, and a newer control plane naming one this build has never
		// heard of must not cost the whole instruction.
		if !signer.IsValidIntegrationTarget(t) || seen[t] {
			continue
		}
		seen[t] = true
		out = append(out, t)
	}
	if len(out) == 0 {
		return integrationsNone
	}
	sort.Strings(out)
	return strings.Join(out, ",")
}

// integrationTargets is flattenIntegrations in reverse: the ids to
// write, or nil for either "no instruction" or "asked, none selected"
// (the caller separates those by comparing against integrationsNone).
func integrationTargets(flat string) []string {
	if flat == "" || flat == integrationsNone {
		return nil
	}
	return strings.Split(flat, ",")
}

// validIntegrationTargets keeps the ids an executor may claim to have
// written and drops the rest, applying the same rule flattenIntegrations
// applies to the control plane's instruction: unknown and retired targets
// are ignored rather than rejected, because a CLI newer or older than the
// daemon it is driving is the ordinary state for the seconds around an
// upgrade.
//
// Sorted and de-duplicated so the record on disk is stable across writes,
// which is what WriteSetupIntegrations would do anyway.
func validIntegrationTargets(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, t := range in {
		if !signer.IsValidIntegrationTarget(t) || seen[t] {
			continue
		}
		seen[t] = true
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

// setupExecutorStep is what the elevated CLI last reported about one
// setup step: its phase, the failure detail and declared code if it
// failed, and the live transfer figures for a byte-denominated step.
//
// Byte fields are only overwritten by a report that actually carries
// them, so the terminating `done` post — which has nothing to say about
// bytes — does not blank the last figures the row was drawn from. The
// failure text and code follow the same rule for the same reason
// (waired-agent#131); see NoteExecutor.
type setupExecutorStep struct {
	phase          string
	errText        string
	errCode        string
	completedBytes int64
	totalBytes     int64
	rateBps        int64
}

// executorErrorCode is the §7 code for a failed executor-reported step:
// what the executor declared, or — for an executor that declared nothing
// — whatever this row's own classifier can make of the text
// (waired-agent#135).
//
// The declaration wins because the executor is the only party that knows
// the difference between "no disk space" and "not running as
// administrator": both arrive here as prose, and prose is what the
// classifiers have been guessing from.
func executorErrorCode(st setupExecutorStep, classify func(string) string) string {
	if st.errCode != "" {
		return st.errCode
	}
	return classify(st.errText)
}

// setupProvider is the narrow view of agentInferenceProvider the
// desired-state applier needs; a test fake implements it without an
// engine.
type setupProvider interface {
	// setupEngineState reports (installed, ready) for one engine kind.
	setupEngineState(ctx context.Context, engine string) (installed, ready bool)
	// setupEngineHealth reports whether this engine's failure is LATCHED —
	// the daemon tried, backed off, retried its budget and gave up — plus the
	// reason it recorded.
	//
	// Latched, not "the last probe failed", on purpose: an engine is briefly
	// unhealthy during every model switch and every restart, and painting the
	// wizard's engine row red for those would be worse than the bug this
	// fixes. Only a give-up is a statement about the install itself (#330).
	setupEngineHealth(ctx context.Context, engine string) (latchedFailure bool, lastError string)
	// setupStateDir is the agent's state root, published to the executor
	// so a bundled engine lands where this daemon will look for it.
	setupStateDir() string
	// setupServingEngine names the engine kind this device serves from.
	// It is the observation that stands in for an instruction on a host
	// nobody set up from a browser (waired-agent#646): the control plane's
	// desired columns are written by the management API alone, so a
	// `waired init` run from a terminal leaves them empty and the desired
	// triple says nothing about what happened here.
	//
	// Always a kind, never "" — the historical ollama default — so it is
	// not on its own evidence that an engine exists. Callers pair it with
	// setupEngineState, which asks the disk.
	setupServingEngine() string
	// setupModelState reports one catalog model's lifecycle state plus
	// live pull bytes and any stored failure detail.
	setupModelState(modelID string) (state string, completed, total int64, errText string)
	// setupHostSpeedProgress reports how far the install-time measurement
	// has got, for its two rows (waired#1143). The reporter could not see
	// that work at all before this: it is the daemon's, it runs off the
	// engine bootstrap rather than off desired state, and the only thing
	// that ever left the process was the finished figure.
	setupHostSpeedProgress() hostSpeedProgress
	BenchmarkStatus() management.BenchmarkStatusResponse
	// startSetupBenchmark kicks the single-flight benchmark job at the
	// given generation (waired#835 §12; join semantics from #99 make
	// repeated calls safe).
	startSetupBenchmark(gen int)
	// setupPreferredModelID reports the model this device is currently
	// set to serve, or "" when nothing has been chosen. It is what makes
	// the model step converge on OBSERVABLE state rather than on an
	// in-memory "did I already do this" flag: a daemon restart clears the
	// flag, and re-applying an already-applied choice would bounce the
	// engine on every boot of a host that is already set up.
	setupPreferredModelID() string
	// setupActiveModelID reports the model this device is actually
	// SERVING, or "" when it is serving none. It is the observed
	// counterpart to setupPreferredModelID: that one answers "what was
	// chosen here", this one answers "what is this machine doing", and on
	// a host nobody ever asked they are not the same question.
	//
	// The observed projection needs the second one. `waired init
	// --non-interactive` never puts the model question to anybody, and on
	// macOS the installer starts the daemon before init runs, so the
	// bundled auto-pull is already under way when the interactive picker
	// looks and the picker steps aside for a host that has model history.
	// Both leave a machine that installed an engine, downloaded a model
	// and answers requests — with nothing on record about a choice
	// (waired-agent#753, #756).
	//
	// A selection is only ever COMMITTED over Ready weights — every writer
	// of state.Active gates on it — so this never names a model that has
	// not finished downloading at least once, and a host mid-first-pull
	// stays silent rather than reporting itself half-built. The commit can
	// outlive readiness (re-pulling the active model moves its state back
	// without clearing the selection), which is why the model row reads
	// the model's own state rather than trusting this: that host reports a
	// running download, and a running row denies completion.
	setupActiveModelID() string
	// setupCanonicalModelID turns a model name from OUTSIDE this process —
	// the control plane's desired_model_id — into the id this device keys
	// its own state by: an alias becomes its manifest's model_id, and a
	// retired entry becomes its successor (#200). An unresolvable name
	// (and "") is returned unchanged, degrading to exactly the compare the
	// caller would have made anyway.
	//
	// It exists because the convergence test in Apply is a raw string
	// compare against setupPreferredModelID(), which reports the id the
	// switch actually PUBLISHED. Any name the two ends spell differently
	// is not a cosmetic mismatch: setupModelState looks the name up in
	// state.Models, which the pull path keys by canonical model_id, so a
	// non-canonical desired value can never read Ready. The wizard's
	// "Download the AI model" row then sits at pending forever, and
	// because modelApplied is in-memory, a restart re-applies the choice
	// and bounces the engine on every boot of a host that is already done.
	//
	// The CLI-side twin is cmd/waired/init_modelselect.go's
	// canonicalBundledModelID.
	setupCanonicalModelID(name string) string
	// setupApplyModel makes modelID the model this device serves — the
	// same operation the operator's own model switch performs (#347/#812),
	// not merely a download. downloading reports whether the weights still
	// had to be fetched; the activation then lands when the pull completes.
	//
	// It replaced a bare PullModel call, which downloaded the wizard's
	// choice and then served something else entirely (#230).
	setupApplyModel(ctx context.Context, modelID string) (downloading bool, err error)
	// PullModel is the fallback for a target the in-process switch cannot
	// apply (a cross-engine change). The weights are fetched now and the
	// activation happens on the next boot, from the preference
	// setupApplyModel already persisted.
	PullModel(ctx context.Context, modelOrAlias string) (management.PullJob, error)
	// startSetupEngine asks the daemon to start an engine that may have
	// been installed after it booted, and to run the post-start bootstrap
	// (#304). Before this existed nothing performed the executor's printed
	// promise of a "next engine start": the daemon resolved the binary once
	// at boot, found none, and stayed inert for the life of the process
	// while the reconciler dispatched `ollama pull` at a server nobody had
	// started.
	//
	// Fire-and-forget, and no ctx by design: the real implementation runs
	// on the daemon's long-lived context because both callers' contexts (an
	// HTTP request handler, a network-map frame) die long before a cold
	// start finishes. reason is for the daemon log.
	startSetupEngine(reason string)
	// setupEnableLocalInference turns local inference on because someone
	// asked this device to serve (#465). A no-op when it is already on.
	//
	// A host below the recommended spec starts with local inference off
	// (waired-ai/waired#1056 decision 4), and since #507 "off" means the
	// engine stands down — so without this the wizard's engine step would
	// be refused on exactly the machines removing the latch finally let
	// the wizard reach. The control plane writes a desired engine or
	// model only when a person chose one, so applying it IS the
	// browser-side half of the opt-in.
	//
	// Fire-and-forget for the same reason startSetupEngine is.
	setupEnableLocalInference(reason string)
	// setupDisableLocalInference is the other half of the operator's
	// explicit local-AI answer (#597; waired#1109/#1110): the same
	// persisted soft toggle a person's own `waired inference off`
	// writes (#465) — a recorded choice, never a fault. A no-op when it
	// is already off. Fire-and-forget, like the enable above.
	setupDisableLocalInference(reason string)
	// setupNoteDesired reports what the control plane's latest frame said
	// about this host — the canonical desired model id ("" when the frame
	// named none) and whether a wizard is driving the host right now — so
	// the boot pre-pull can stand down instead of racing the choice the
	// operator is about to make (#379).
	//
	// Called on EVERY frame, including the ones that carry no instruction
	// at all: "a frame arrived and nobody is driving" is the evidence that
	// releases the hold, and it is only observable from the empty frame.
	// Fire-and-forget, like startSetupEngine.
	setupNoteDesired(modelID string, driving bool)
}

// setupReconciler applies the CP-served desired state (waired#835 §6)
// and reports typed step progress back (§7). Apply is invoked on EVERY
// network-map frame — streaming has no dedup — so every action here
// must be idempotent: convergence is derived from observable state
// (catalog model states, the persisted preference, the persisted
// benchmark generation), never from "did I already do this" flags that
// could desync from reality. The one exception is model admission (one
// setupApplyModel call per desired model value) so a permanently failing
// download is not re-queued on every frame; an agent restart retries it
// once more, and the persisted preference stops that retry from bouncing
// the engine on a host that already converged.
type setupReconciler struct {
	provider   setupProvider
	push       *controlclient.Client // nil = report-nothing (no CP push)
	deviceID   string
	machineKey ed25519.PrivateKey
	logger     *slog.Logger
	now        func() time.Time // test seam
	interval   time.Duration    // push cadence; setupPushInterval outside tests

	mu      sync.Mutex
	desired setupDesired
	active  bool // a desired instruction has been seen this session
	// desiredSeen marks that at least one map frame has been folded here,
	// carrying an instruction or not. It anchors the baseline below.
	desiredSeen bool
	// desiredChangedAt is when THIS daemon watched the instruction change,
	// on its own clock (#308). The control plane never clears
	// desired_engine / desired_model_id, so their mere presence says
	// nothing about whether a wizard is driving: a device set up once
	// carries them for the rest of its life. What does say so is having
	// watched them change — which is why the first frame after boot is
	// treated as a snapshot of whatever was already there, not as a write.
	//
	// Zero means "never watched one change", i.e. everything we know is
	// leftovers.
	desiredChangedAt time.Time
	modelApplied     map[string]bool // one setupApplyModel call per desired model value
	// modelAdmitted is the last desired model value this process spent an
	// admission on. It is what tells a REPEAT of an instruction from a
	// RETURN to an earlier one (waired-agent#779).
	//
	// modelApplied alone cannot: keyed on the value, it says "this process
	// applied that model once", which stays true after the device has
	// moved on to something else. An operator switching A -> B -> A then
	// found the second change dropped, and dropped silently — a spent
	// admission falls off both arms of the model step below, so there was
	// no apply and no "leaving it alone" line either. Both hosts of the
	// rc9 3-OS run reproduced it.
	//
	// A model whose admission ended in a REFUSAL keeps it. modelRejected
	// is what says so, and re-admitting on the strength of "the device is
	// serving something else" would re-queue a genuinely failing download
	// on every frame — the case TestSetupPullNotReadmittedWithoutEngineTransition
	// pins. Recovery from a refusal stays the retry generation (#136) and
	// the engine-appeared edge, exactly as before.
	modelAdmitted string
	// leftoverNoted is one log line per desired model value this daemon
	// declined to apply because nobody here chose it (#626). Keyed the
	// same way modelApplied is, and for the same reason: the control
	// plane re-sends its instruction on every map frame.
	leftoverNoted map[string]bool
	// modelRejected records why applying the desired model was refused
	// (an unknown model, an engine that cannot serve it, a host whose
	// pulls are turned off). It feeds the model step's failure rather
	// than leaving it pending.
	modelRejected map[string]setupModelRejection
	kick          chan struct{} // wakes the push loop on Apply changes

	// Executor lease (§9/§11). The elevated CLI from `sudo waired init`
	// heartbeats here; a stale lease is what turns an install step into
	// executor_gone instead of a spinner. everSeen distinguishes "the
	// executor died" (recoverable — re-run the command) from "no executor
	// ever showed up" (permission_denied).
	executorAttached bool
	executorEverSeen bool
	executorElevated bool
	executorSeen     time.Time
	// executorSteps is the lease's phase and byte progress PER STEP
	// (#197). It used to be one phase for the whole lease, which was
	// enough while the executor drove exactly one row; now that the
	// engine download and the install proper are separate rows — and
	// waired#935 adds the integration — a single field would let the
	// install's phase erase the finished download's.
	executorSteps map[string]setupExecutorStep
	// integrationsWritten is the coding-tool instruction an executor has
	// reported DONE on this device, read from the daemon state dir at
	// construction and re-written on every accepted `done`
	// (waired-agent#312).
	//
	// It is the only part of the §7 projection that cannot be re-derived
	// by looking at the machine: the engine and model rows stat the disk
	// and probe the engine, but the files this step writes live in the
	// invoking user's home and in root-owned managed settings, and the
	// daemon deliberately has no business reading either. Persisting the
	// executor's report is the nearest observable thing there is — and
	// without it every service restart walked a finished device back to
	// "nobody has run the setup command here".
	integrationsWritten state.SetupIntegrations
	// inferenceActed is the last DesiredInference value acted on here,
	// read from the daemon state dir at construction and re-written on
	// every application (#597). Durable for the reason the record's own
	// doc states: the CP re-sends the instruction on every frame and
	// never clears it, so without the marker every restart would
	// re-apply a weeks-old wizard answer over a person's later local
	// `waired inference off|on` — the silent revert #465 forbids.
	inferenceActed state.SetupInference
	// executorDriver is the surface a live lease claims to be driving
	// (waired-agent#198) — in practice only ever "terminal". Bound to the
	// lease exactly like installClaimed below: a claim that outlived its
	// executor would have the wizard reporting a terminal that is not
	// running, with no way back.
	executorDriver string
	// installClaimed names the engine whose install a live lease claimed.
	// Bound to the LEASE, never to desired_engine: a claim that outlived
	// its executor would make the "re-run sudo waired init" recovery a
	// no-op and would let one local POST block installation forever.
	installClaimed string

	// Last observed engine-installed state, used to detect the
	// false->true transition that re-admits a model pull which failed
	// only because there was no engine to pull with.
	engineInstalled bool
	engineObserved  bool

	// modelGenActed is the highest desired_model_gen this process has
	// acted on (#136). It is the answer to the CP's request, the same
	// way the persisted benchmark generation answers desired_benchmark_gen
	// — and it is echoed in every snapshot so the wizard can tell "not
	// picked up yet" from "picked up and failed again".
	//
	// In memory only, like modelApplied itself: a restart re-admits the
	// pull anyway (that is the ONLY recovery this issue exists to
	// replace), so persisting it would buy nothing and could suppress a
	// re-admission the operator asked for before the crash.
	modelGenActed int

	// stateDir is where the integration record lives. Captured at
	// construction rather than asked of the provider per write, so the
	// file path never depends on when the question is asked.
	stateDir string
}

func newSetupReconciler(provider setupProvider, push *controlclient.Client, deviceID string, machineKey ed25519.PrivateKey, logger *slog.Logger) *setupReconciler {
	r := &setupReconciler{
		provider:      provider,
		push:          push,
		deviceID:      deviceID,
		machineKey:    machineKey,
		logger:        logger,
		now:           time.Now,
		interval:      setupPushInterval,
		modelApplied:  map[string]bool{},
		modelRejected: map[string]setupModelRejection{},
		leftoverNoted: map[string]bool{},
		executorSteps: map[string]setupExecutorStep{},
		kick:          make(chan struct{}, 1),
		stateDir:      provider.setupStateDir(),
	}
	if r.stateDir == "" {
		// Nowhere to keep the record. The daemon always has a state dir
		// (--state-dir has a per-OS default), so this is not a state any
		// deployment reaches — but resolving it to a RELATIVE path would
		// scatter the file across whatever directory the process happened
		// to start in, which is worse than not having it.
		return r
	}
	// Read once, here, rather than per snapshot: this process is the only
	// writer, so the cache cannot go stale, and snapshot() runs on the
	// push loop every couple of seconds.
	written, err := state.ReadSetupIntegrations(r.stateDir)
	if err != nil && logger != nil {
		// Not fatal. A record we cannot parse is indistinguishable from no
		// record for every purpose but this log line: the row falls back to
		// the arms it used before the record existed, which is the honest
		// answer when the evidence is unreadable.
		logger.Warn("setup: cannot read the coding-tools record", "err", err)
	}
	r.integrationsWritten = written
	acted, err := state.ReadSetupInference(r.stateDir)
	if err != nil && logger != nil {
		// Same posture as above — with the one consequence worth naming:
		// an unreadable record means a replayed instruction acts once
		// more, which is idempotent against the persisted toggle unless a
		// person flipped it since.
		logger.Warn("setup: cannot read the local-inference answer record", "err", err)
	}
	r.inferenceActed = acted
	return r
}

// Apply reconciles toward the desired state on the device's Self map
// entry. Called from streaming on every frame; hosts that never ran a
// NAVI setup take the zero-value fast path and do no work at all.
func (r *setupReconciler) Apply(ctx context.Context, st *signer.InferenceState) {
	if r == nil || st == nil {
		return
	}
	d := setupDesired{
		engine: st.DesiredEngine,
		// Canonicalised ONCE, here, so everything downstream speaks one id:
		// the modelApplied / modelRejected keys, setupModelState, the
		// convergence compare, setupApplyModel, and the SetupState echo the
		// CLI watcher reads back. See setupCanonicalModelID.
		modelID:      r.provider.setupCanonicalModelID(st.DesiredModelID),
		benchmarkGen: st.DesiredBenchmarkGen,
		modelGen:     st.DesiredModelGen,
		integrations: flattenIntegrations(st.DesiredIntegrations),
		inference:    st.DesiredInference,
	}
	r.mu.Lock()
	// The baseline is the first frame folded here, whatever it carries —
	// deliberately BEFORE the zero-value fast path below. A device that
	// has never been set up spends its first frames in that fast path, so
	// anchoring later would file the wizard's very first instruction as a
	// leftover: browser-driven setup, reported as nothing happening.
	baseline := !r.desiredSeen
	r.desiredSeen = true
	if d == (setupDesired{}) && !r.active {
		r.mu.Unlock()
		// Nobody is driving. This read the executor lease until #540, on the
		// reasoning that a wizard can be driving before the control plane has
		// written a single desired value (#379) — see the comment on driving
		// below for why the lease does not answer that question.
		//
		// Reported rather than returned silently: this frame is the one
		// that says "the control plane answered and nobody is driving",
		// which is the only evidence that releases the boot pre-pull hold
		// on a host that will never be set up from a browser.
		r.provider.setupNoteDesired("", false)
		return
	}
	changed := d != r.desired
	// The serve-ask below must not read an inference-only change as
	// "asked to serve" (#597): a wizard writing "off" beside a standing
	// engine would otherwise fire an enable a breath before the off
	// applies. Compared with the inference field blanked on both sides,
	// which is exactly the comparison `changed` made before the field
	// existed.
	prevServe, dServe := r.desired, d
	prevServe.inference, dServe.inference = "", ""
	changedServe := dServe != prevServe
	if changed && !baseline {
		// Watched it change: something wrote this instruction while we
		// were here (#308).
		r.desiredChangedAt = r.now()
	}
	r.desired = d
	r.active = true
	// Retry (#136): a generation ahead of the one we last acted on is the
	// operator asking for the download again, and the answer is the same
	// clearing the engine-appeared transition does below — drop the
	// one-shot admission for this model so the pull is re-queued once.
	//
	// It has to happen HERE, not beside `appeared`, because that block is
	// skipped entirely when there is no desired engine, and a retry has to
	// work on a host that never needed one. Recording modelGenActed in the
	// same critical section is what makes a re-bump of the SAME generation
	// a no-op: the CP re-sends its instruction on every map frame, so a
	// condition on the value alone would re-queue forever.
	retried := d.modelGen > r.modelGenActed
	if retried {
		r.modelGenActed = d.modelGen
		if d.modelID != "" {
			delete(r.modelApplied, d.modelID)
			delete(r.modelRejected, d.modelID)
			delete(r.leftoverNoted, d.modelID)
		}
	}
	// A RETURN to a model this process already spent its admission on is a
	// new instruction, not a repeat (#779) — see modelAdmitted. Restoring
	// the admission here rather than at the apply site keeps it beside the
	// retry above, which is the same operation for the same reason.
	//
	// leftoverNoted is deliberately NOT restored: it is one line per value
	// this daemon declined, and modelAdmitted is empty for a leftover (no
	// admission was ever spent), so clearing it here would re-log on every
	// frame — the per-frame noise #626's note exists to avoid.
	if d.modelID != "" && d.modelID != r.modelAdmitted {
		if _, refused := r.modelRejected[d.modelID]; !refused {
			delete(r.modelApplied, d.modelID)
		}
	}
	// #379: r.active is true by now, so "a wizard is driving" is the
	// freshness test #308 already uses — an instruction we watched change is
	// someone writing it while we were here, and one we only ever read back
	// is the control plane replaying a device set up weeks ago.
	//
	// The executor lease used to be OR'd in here, and read on its own in the
	// fast path above. It is not evidence of a wizard (#540). `waired init`
	// is the only thing that takes it (attachSetupExecutor) and the only
	// thing that installs an engine, and it keeps the lease through the model
	// wait it does AFTER the install — so the boot pre-pull's hold waited for
	// the process that was waiting for the hold, twenty minutes at a time, on
	// every non-interactive install. Nor is the lease needed for the case it
	// was added for: the browser cannot reach an engine install without
	// desired state, because setupEngineInstallWanted gates the executor on
	// setupDriving, which reads st.Active. Serving only waired-managed
	// engines (#488) is what settled that; before it, a foreign engine
	// already on disk could put a host here with nothing written.
	//
	// leaseLiveLocked also expires a dead lease as a side effect. Dropping
	// the call does not defer that: snapshot() runs it on the push loop every
	// setupPushInterval — started beside this reconciler, so it runs wherever
	// Apply does — and SetupState runs it on every executor poll.
	driving := !r.desiredStaleLocked()
	r.mu.Unlock()
	r.provider.setupNoteDesired(d.modelID, driving)
	if retried && r.logger != nil {
		r.logger.Info("setup: retry requested; re-admitting the desired model",
			"gen", d.modelGen, "model", d.modelID)
	}

	// Someone asked this device to serve (#465). Gated on `changedServe`
	// — not on the current toggle state — because Apply runs on every
	// frame and the control plane never clears a desired value:
	// re-asserting on every frame would undo a `waired inference off`
	// seconds after the user made it. A benchmark generation alone is
	// deliberately not an ask; it tells a device that already serves to
	// measure itself. And an inference-only change is the OPPOSITE of an
	// ask (#597) — it is excluded from this comparison and applied below.
	if changedServe && (d.engine != "" || d.modelID != "") {
		r.provider.setupEnableLocalInference("setup: the wizard asked this device to serve")
	}

	// The operator's explicit local-AI answer (#597; waired#1109/#1110),
	// applied once per persisted value — see applyDesiredInference. AFTER
	// the serve-ask above, so a frame that (incoherently) carries both a
	// serve instruction and "off" lands on off — the explicit answer
	// outranks the implied one, and the CP validates the pair anyway.
	r.applyDesiredInference(d.inference)

	// Benchmark (§12). See startBenchmarkIfDue.
	r.startBenchmarkIfDue(d)

	// Engine (§11): the agent cannot install one unprivileged — that is
	// the executor's job. Apply does two things with it.
	//
	// First, it gates the model step below: until the engine is actually
	// installed there is nothing to serve the model FROM, so applying the
	// choice can only fail. enginePresent carries that answer forward.
	//
	// Second, it watches for the engine APPEARING, because that
	// transition invalidates the one-shot admission. With the gate in
	// place the common engine-less case never burns the attempt, but an
	// engine that goes away and comes back (a reinstall, a profiler
	// cache that briefly reports it missing) still has to re-admit, or
	// the download stays red for the rest of the process's life. Keyed
	// on the transition, not on every frame, so a genuinely failing
	// download is still not re-queued in a loop.
	enginePresent := d.engine == ""
	if d.engine != "" {
		installed, _ := r.provider.setupEngineState(ctx, d.engine)
		enginePresent = installed
		if r.noteEngineInstalled(installed, d.modelID) {
			// noteEngineInstalled has already dropped the admission record
			// itself, under the same lock as the latch, so the step below
			// re-reads it rather than carrying a local copy.
			if d.modelID != "" {
				changed = true
			}
			r.onEngineAppeared(d.engine, d.modelID)
		}
	}

	// Model (§6: catalog IDs only — the provider resolves against the
	// catalog and refuses anything it doesn't know).
	//
	// This APPLIES the choice; it does not merely download it. Until
	// #230 the desired model was handed to PullModel and nowhere else,
	// so the wizard's choice was fetched — tens of gigabytes of it — and
	// then the daemon went on serving whatever it had auto-selected for
	// itself. Neither activation path could rescue it:
	// activateBundledIfUnset only fires for the install-time bundled
	// model, and activatePreferredIfNeeded needs a preference the setup
	// path never wrote. Worse, a choice whose weights were ALREADY on
	// disk skipped the pull condition entirely and therefore did nothing
	// at all. Applying it is the operator's own model switch (#347/#812):
	// persist the preference, then flip the active selection.
	//
	// Admission is once per desired model value, and never before the
	// engine that would serve it exists (see enginePresent above).
	// Holding it back is what lets the wizard write the engine and the
	// model in one gesture: the step sits at `pending` for the length of
	// the install instead of showing a failure that resolves itself
	// (waired#904).
	//
	// A device that has already CONVERGED — the desired model is the one
	// it is set to serve, and its weights are on disk — is left alone, so
	// a daemon restart re-reading the same instruction does not bounce a
	// healthy engine on every boot. Convergence needs both halves: a
	// preference alone is satisfied the instant the switch is published,
	// which would make the engine-reappears retry below a no-op and leave
	// a failed download red for the rest of the process's life.
	// And never for an instruction nobody here asked for. `driving` is the
	// #308 freshness test this function already computes and, until #626,
	// only ever spent on the boot pre-pull's hold: an instruction this
	// daemon WATCHED change is someone writing it while we were here, and
	// one we only ever read back is leftovers.
	//
	// It has to gate the apply too, because a leftover is not a stale
	// opinion — it starts a multi-gigabyte download of a model nobody
	// chose. The control plane never clears desired_model_id (see
	// desiredChangedAt), a re-enrolment reuses the device row without
	// touching it (waired-ai/waired#1136), and a fresh install arrives
	// with an empty state dir, so `applied` and `converged` are both
	// false by construction: everything that could have said "no" was
	// missing at once. On the rc8 macOS host that combination applied a
	// July instruction to an August install, downloaded 3.4 GB, and the
	// benchmark then backed the whole thing out — a second download to
	// return to the model the agent had picked for itself in the first
	// line of its log.
	//
	// A retry is an ask (#136): bumping desired_model_gen is the operator
	// saying "try it again", which is a person acting now even when the
	// value did not change.
	//
	// Not a refusal, and deliberately not fit-shaped. Capacity is the only
	// rule allowed to refuse a model and it does not refuse this one
	// (waired-ai/waired#1067, 2026-08-08); what is missing here is not
	// memory but consent, and the answer is to leave the question to the
	// surfaces that can ask a person. Declining keeps
	// modelQuestionUnanswered standing, so the install-flow picker (#586)
	// still runs and the browser can still write a choice — which arrives
	// as a watched change and applies through this same branch.
	r.stepDesiredModel(ctx, d.modelID, enginePresent, driving || retried)

	if changed {
		r.kickPush()
	}
}

// stepDesiredModel is the model step of Apply, reachable from the
// reconcile pass as well so convergence does not depend on a control-plane
// frame arriving (#779). Both callers hold no lock.
//
// `asked` folds the two things that make an instruction actionable here:
// a wizard driving (the #308 freshness test) or an explicit retry (#136).
func (r *setupReconciler) stepDesiredModel(ctx context.Context, modelID string, enginePresent, asked bool) {
	if modelID == "" || !enginePresent {
		return
	}
	r.mu.Lock()
	applied := r.modelApplied[modelID]
	r.mu.Unlock()
	if applied {
		return
	}
	if !asked {
		// Once per model value, not once per frame: the control plane
		// re-sends its instruction on every map frame, and a line per
		// frame would bury the one that matters.
		if !r.noteLeftoverDesired(modelID) && r.logger != nil {
			r.logger.Info("setup: leaving the desired model alone; nobody here chose it this install",
				"model", modelID,
				"hint", "pick one with `waired models pull <model>` or from the browser dashboard")
		}
		return
	}
	state, _, _, _ := r.provider.setupModelState(modelID)
	if state == catalog.ModelStateReady && r.provider.setupPreferredModelID() == modelID {
		return // converged
	}
	r.mu.Lock()
	r.modelApplied[modelID] = true
	r.modelAdmitted = modelID
	r.mu.Unlock()
	if _, err := r.provider.setupApplyModel(ctx, modelID); err != nil {
		r.mu.Lock()
		// Classified HERE, where the error value still exists.
		// Storing only the text and re-deriving a code from it in
		// snapshot() is what collapsed every refusal into
		// model_not_found (waired-agent#134).
		r.modelRejected[modelID] = setupModelRejection{
			code:   classifyModelRejection(err),
			detail: err.Error(),
		}
		r.mu.Unlock()
		if r.logger != nil {
			r.logger.Warn("setup: desired model refused", "model", modelID, "err", err)
		}
	}
}

// reconcileDesiredModel gives the standing instruction a second look on
// the reporter's own schedule, so "desired != what this device serves"
// converges without waiting for a control-plane frame that nothing local
// can cause (#779).
//
// It is the model-step twin of what snapshot() already does for the engine
// rows, and for the same stated reason (the #413 comment beside that
// probe): Apply runs only when a frame arrives, so an edge that happens
// between frames — the engine binary landing, a pull finishing — had no
// reader. Whichever probe notices first owns the transition; the latch in
// noteEngineInstalled makes sure only one does.
//
// It adds no goroutine and no timer: runPush already ticks beside this
// reconciler and already holds both halves of the comparison.
func (r *setupReconciler) reconcileDesiredModel(ctx context.Context) {
	if r == nil {
		return
	}
	r.mu.Lock()
	if !r.active {
		r.mu.Unlock()
		return
	}
	d := r.desired
	driving := !r.desiredStaleLocked()
	r.mu.Unlock()
	if d.modelID == "" {
		return
	}
	enginePresent := d.engine == ""
	if d.engine != "" {
		installed, _ := r.provider.setupEngineState(ctx, d.engine)
		enginePresent = installed
		if r.noteEngineInstalled(installed, d.modelID) {
			r.onEngineAppeared(d.engine, d.modelID)
			r.kickPush()
		}
	}
	// No retry term: a generation bump arrives on a frame, and Apply acts
	// on it there. This pass only ever finishes work already admitted in
	// principle.
	r.stepDesiredModel(ctx, d.modelID, enginePresent, driving)
}

// reconcileBenchmark gives a standing benchmark request a second look on
// the reporter's own 2 s tick, for the reason reconcileDesiredModel above
// exists: Apply runs only when a network-map frame arrives, and the edge
// this request waits for — a model download finishing — moves no map
// epoch. Left on frames alone, a request made before the download would
// have no reader at the moment it finally became runnable.
func (r *setupReconciler) reconcileBenchmark() {
	if r == nil {
		return
	}
	r.mu.Lock()
	active := r.active
	d := r.desired
	r.mu.Unlock()
	if !active {
		return
	}
	r.startBenchmarkIfDue(d)
}

// startBenchmarkIfDue starts the measurement the served generation
// counter is asking for, once there is something to measure (§12).
//
// The counter is the request; the persisted last-completed generation is
// the answer. A run that FAILED at the requested gen is still an answer
// (the error rides setup-progress; NAVI re-bumps to retry), so only a
// genuinely behind, not-running job starts one.
//
// benchmarkTargetReady is what makes the request survivable when it is
// made EARLY — which is now the normal case, because the wizard asks
// about the speed check on the model step and writes the answer with the
// choice rather than offering a button after the download
// (waired-ai/waired#1247). Without it the request is spent on a host that
// has nothing to measure yet, and spent means gone: RunBootBenchmark
// answers `skipped` when there is no engine port to talk to, `skipped` is
// a RECORDED ending (only engine_not_ready is not — see runBenchmarkJob),
// and a recorded ending at the requested generation satisfies the guard
// below forever. The measurement then never runs and the wizard shows a
// finished speed check with no figure in it.
func (r *setupReconciler) startBenchmarkIfDue(d setupDesired) {
	if d.benchmarkGen <= 0 {
		return
	}
	if !r.benchmarkTargetReady(d.modelID) {
		return
	}
	bs := r.provider.BenchmarkStatus()
	if bs.State != management.BenchmarkStateRunning && bs.Gen < d.benchmarkGen {
		r.provider.startSetupBenchmark(d.benchmarkGen)
	}
}

// benchmarkTargetReady reports whether this host has something to
// measure for the standing request.
//
// No desired model means the request is about whatever this host already
// serves — a benchmark asked for from the device page, or on a host set
// up from a terminal — and there is nothing to wait for.
//
// With one, BOTH halves are required and they are different questions.
// setupActiveModelID is what the machine is SERVING, which is what the
// benchmark measures (BenchDeps.EngineModel comes from the same active
// selection): a model whose weights have arrived but which has not been
// switched to yet would be timed as the model it replaced. setupModelState
// is whether those weights are on disk right now, and it is needed as
// well because a selection outlives readiness — re-pulling the active
// model moves its state back without clearing the selection, which the
// provider's own doc for setupActiveModelID records.
func (r *setupReconciler) benchmarkTargetReady(modelID string) bool {
	if modelID == "" {
		return true
	}
	if r.provider.setupActiveModelID() != modelID {
		return false
	}
	state, _, _, _ := r.provider.setupModelState(modelID)
	return state == catalog.ModelStateReady
}

// noteLeftoverDesired records that this daemon declined to apply
// modelID, and reports whether it had already said so.
//
// A retry clears the record along with modelApplied, so an operator who
// bumps the generation gets the line again if the instruction is still
// leftovers by then.
func (r *setupReconciler) noteLeftoverDesired(modelID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.leftoverNoted[modelID] {
		return true
	}
	r.leftoverNoted[modelID] = true
	return false
}

// noteEngineInstalled records an engine-presence observation against the
// latch and reports whether THIS call is the false->true edge — the
// transition that invalidates the one-shot model admission.
//
// It exists because two readers probe the engine independently and used
// to disagree about when it appeared. Apply probes on a control-plane
// frame; snapshot() probes on its own 2 s ticker. Only Apply updated the
// latch, so between the binary landing on disk and the next frame,
// snapshot() reported the engine rows `done` while modelApplied /
// modelRejected still held the failure from the engine-less attempt —
// and the model row showed that stale failure for a window bounded by
// control-plane frame cadence, which is not observable from this repo
// (waired-agent#413).
//
// Both callers go through here now, so whichever notices first owns the
// edge and the other sees an already-updated latch. Clearing the
// admission records here rather than at the call sites is deliberate:
// they are what the edge invalidates, and doing it under the same lock
// as the latch means no reader can observe "engine appeared" and the old
// rejection together.
func (r *setupReconciler) noteEngineInstalled(installed bool, modelID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	appeared := installed && r.engineObserved && !r.engineInstalled
	r.engineInstalled = installed
	r.engineObserved = true
	if appeared && modelID != "" {
		delete(r.modelApplied, modelID)
		delete(r.modelRejected, modelID)
	}
	return appeared
}

// onEngineAppeared is what both probes do once the edge is theirs.
//
// Re-admitting the model without an engine to pull WITH is the other
// half of #304: `ollama pull` is a client of a server nobody started.
// Not gated on modelID — an engine that just appeared is worth starting
// either way. This is the observable-state backstop for an executor that
// died mid-install or a daemon that restarted while the wizard was
// running.
func (r *setupReconciler) onEngineAppeared(engine, modelID string) {
	if r.logger != nil {
		r.logger.Info("setup: engine became installed; re-admitting the desired model",
			"engine", engine, "model", modelID)
	}
	r.provider.startSetupEngine("setup: engine binary appeared")
}

// kickPush wakes the reporter loop so a state change reaches NAVI on the
// next push rather than on the next tick boundary.
func (r *setupReconciler) kickPush() {
	select {
	case r.kick <- struct{}{}:
	default:
	}
}

// leaseLiveLocked reports whether the executor lease is still fresh and,
// when it is not, drops the lease-bound install claim. Callers hold mu.
// desiredStaleLocked reports whether the instruction we hold is left over
// from an earlier run rather than something a wizard is driving now
// (#308). Caller holds r.mu.
//
// Two ways to be stale, and the first is the one that bites: never having
// watched it change at all — the control plane persists desired state
// forever, so a device set up weeks ago hands its daemon a full
// instruction on the first frame after boot.
func (r *setupReconciler) desiredStaleLocked() bool {
	if r.desiredChangedAt.IsZero() {
		return true
	}
	return r.now().Sub(r.desiredChangedAt) > setupDesiredFreshWindow
}

func (r *setupReconciler) leaseLiveLocked() bool {
	if !r.executorAttached {
		return false
	}
	if r.now().Sub(r.executorSeen) > setupExecutorTTL {
		r.executorAttached = false
		r.installClaimed = ""
		r.executorDriver = ""
		return false
	}
	return true
}

// NoteExecutor records one lease heartbeat or release from the elevated
// CLI (§9/§11) and returns the resulting state, so the executor learns
// the install claim in the same round trip.
func (r *setupReconciler) NoteExecutor(ctx context.Context, req management.SetupExecutorRequest) management.SetupStateResponse {
	if r == nil {
		return management.SetupStateResponse{}
	}
	r.mu.Lock()
	phase := req.Phase
	if phase == "" {
		phase = management.SetupExecutorPhaseIdle
	}
	// An unnamed step is the engine install: that is all a lease could
	// report before the split, and all it ever meant.
	stepID := req.Step
	if stepID == "" {
		stepID = setupStepEngineInstall
	}
	st := r.executorSteps[stepID]
	prevPhase := st.phase
	st.phase = phase
	// A report that says nothing about the failure must not erase the
	// reason we already have (waired-agent#131). Two posts do exactly
	// that: the heartbeat repeats the phase every 10 s with an empty
	// Error, and Release posts one last time on the way out — so the
	// detail the executor sent with Failed() survived at most ten
	// seconds, and classifySetupFailure("") then called every failed
	// install a network error, whatever had actually gone wrong.
	//
	// Anything that moves the step off `failed` clears both fields
	// normally, so a stale reason cannot outlive the failure it belongs
	// to.
	if req.Error != "" || req.ErrorCode != "" || phase != management.SetupExecutorPhaseFailed {
		st.errText = req.Error
		st.errCode = req.ErrorCode
	}
	if req.CompletedBytes > 0 || req.TotalBytes > 0 {
		st.completedBytes = req.CompletedBytes
		st.totalBytes = req.TotalBytes
		st.rateBps = req.RateBps
	}
	r.executorSteps[stepID] = st
	switch {
	case req.StepOnly:
		// A report from a process that holds no lease and is not asking
		// for one: `waired link` saying the coding tools it just repaired
		// are in place (waired-agent#791). The step record above and the
		// edges below are all it gets; the lease, the driver claim, the
		// install claim and the elevation flag are somebody else's facts
		// and none of them is being asserted here.
		//
		// That distinction is not decoration. executorElevated outlives
		// the lease on purpose, so engine_install can report
		// permission_denied for an unprivileged executor that came and
		// went; an ordinary non-root `waired link` refreshing it would
		// turn that row red on a host whose engine an elevated `waired
		// init` had installed perfectly well.
	case req.Attached:
		r.executorAttached = true
		r.executorEverSeen = true
		r.executorElevated = req.Elevated
		r.executorSeen = r.now()
		// Empty leaves the claim alone: a heartbeat does not have to
		// repeat it, and nothing else may quietly drop it.
		if req.Driver != "" {
			r.executorDriver = req.Driver
		}
		// Only an engine step moves the install claim. An integration
		// report (waired#935) rides the same lease and must not hand the
		// engine install to a second executor by reporting `done`.
		if management.IsEngineSetupStep(req.Step) {
			switch phase {
			case management.SetupExecutorPhaseInstalling:
				if req.Engine != "" {
					r.installClaimed = req.Engine
				}
			case management.SetupExecutorPhaseDone, management.SetupExecutorPhaseFailed:
				// The attempt is over either way; a fresh executor (or this
				// one, after the operator fixes whatever failed) may claim it
				// again.
				//
				// Except while the download is still the step being
				// reported: `engine_download: done` means the bytes are
				// here and the install proper is next, in the SAME lease.
				// Dropping the claim there would invite a second elevated
				// install of the engine this one is mid-way through.
				if stepID != setupStepEngineDownload || phase != management.SetupExecutorPhaseDone {
					r.installClaimed = ""
				}
			}
		}
	default:
		// Explicit release — same effect as the lease expiring, minus the
		// TTL wait, so Ctrl-C surfaces as executor_gone promptly.
		r.executorAttached = false
		r.installClaimed = ""
		r.executorDriver = ""
	}
	// #304: the executor has finished installing the engine, so perform
	// the "next engine start" it just promised the operator — the daemon
	// resolved the binary once at boot and would otherwise stay inert for
	// the rest of the process.
	//
	// Strictly an EDGE. Done(engine) sets phase=done with an empty step,
	// and the executor then re-posts that same pair every 10 s for as long
	// as it stays attached — which is the whole model download, up to the
	// 8 h residency budget. A level trigger here would fire thousands of
	// times per session.
	//
	// engine_download:done is excluded: the bytes are here but the install
	// proper is next in the same lease, which is why the install claim is
	// deliberately kept there too.
	startEngine := stepID == setupStepEngineInstall &&
		phase == management.SetupExecutorPhaseDone &&
		prevPhase != management.SetupExecutorPhaseDone
	// waired-agent#312: the coding tools are written, so record WHAT was
	// written before the lease — and with it the only evidence this row
	// has — goes away. The same edge discipline as startEngine above, for
	// the same reason: the heartbeat re-posts a terminal phase every 10 s.
	//
	// The targets come from the instruction as the daemon currently holds
	// it, not from the report: the executor applies what SetupState served
	// it, and that is this value. There is no field on the wire for it to
	// echo back, and inventing one would only let the two disagree.
	var recordIntegrations []string
	if stepID == setupStepIntegration &&
		phase == management.SetupExecutorPhaseDone &&
		prevPhase != management.SetupExecutorPhaseDone {
		// "Asked, and every toggle was off" writes nothing and needs no
		// record: that row is served as `skipped` from the instruction
		// itself, on every boot, without an executor.
		recordIntegrations = integrationTargets(r.desired.integrations)
		if len(recordIntegrations) == 0 && r.desired.integrations == "" {
			// No instruction to read back. That is the ordinary state of a
			// terminal-driven init (waired-agent#646): the desired columns
			// are the management API's, so `waired init` never populates
			// them, and the coding tools it wrote would go unrecorded — the
			// row would then be missing from the report for the rest of the
			// device's life, on a machine where it demonstrably happened.
			//
			// The executor names them, and only here. While an instruction
			// exists it stays the authority, so the two still cannot
			// disagree about a row the daemon serves — which is what the
			// comment above is protecting.
			recordIntegrations = validIntegrationTargets(req.IntegrationTargets)
		}
	}
	r.mu.Unlock()
	if startEngine {
		r.provider.startSetupEngine("setup: executor reported the engine install done")
	}
	if len(recordIntegrations) > 0 {
		r.recordIntegrationsWritten(recordIntegrations)
	}
	r.kickPush()
	return r.SetupState(ctx)
}

// recordIntegrationsWritten persists the coding-tool outcome and caches it
// for the projection (waired-agent#312). Called off the lock: this is a
// file write, and NoteExecutor runs on every 10 s heartbeat of every lease.
//
// A write that fails is logged and dropped, never propagated. The step it
// describes already succeeded, the caller is an executor heartbeat with no
// use for the error, and the in-memory phase still reports `done` for the
// life of this process — the only thing lost is the answer after a restart,
// which is the state every build before this one shipped with.
func (r *setupReconciler) recordIntegrationsWritten(targets []string) {
	if r.stateDir == "" {
		return // see newSetupReconciler
	}
	rec := state.SetupIntegrations{
		Targets:   targets,
		WrittenAt: r.now().UTC().Format(time.RFC3339),
	}
	if err := state.WriteSetupIntegrations(r.stateDir, rec); err != nil {
		if r.logger != nil {
			r.logger.Warn("setup: cannot record the coding-tools outcome",
				"targets", targets, "err", err)
		}
		return
	}
	r.mu.Lock()
	r.integrationsWritten = rec
	r.mu.Unlock()
	if r.logger != nil {
		r.logger.Info("setup: recorded the coding tools this device has connected",
			"targets", targets)
	}
}

// SetupState projects what a setup executor needs in order to decide
// whether to act. Everything here is derived from observable state.
func (r *setupReconciler) SetupState(ctx context.Context) management.SetupStateResponse {
	if r == nil {
		return management.SetupStateResponse{}
	}
	r.mu.Lock()
	d := r.desired
	resp := management.SetupStateResponse{
		Active:              r.active,
		DesiredStale:        r.active && r.desiredStaleLocked(),
		DesiredEngine:       d.engine,
		DesiredModelID:      d.modelID,
		DesiredBenchmarkGen: d.benchmarkGen,
	}
	if r.leaseLiveLocked() {
		resp.ExecutorAttached = true
		resp.ExecutorElevated = r.executorElevated
	}
	resp.InstallClaimed = r.installClaimed
	// The refusal, read under the same lock and from the same map the
	// pushed snapshot reads (#404). Keyed on the CURRENT desired model, so
	// an operator who picks another one is not shown the last one's answer.
	rejected := r.modelRejected[d.modelID]
	if d.integrations != "" {
		targets := integrationTargets(d.integrations)
		if targets == nil {
			targets = []string{} // asked, nothing selected
		}
		resp.Integrations = &targets
		// The same question integrationStep's first arm asks, answered
		// for the executor: is there anything here still to write? An
		// all-off instruction (empty targets) is satisfied by definition,
		// and a record that covers the current targets means an earlier
		// run already did it (waired-agent#312, #987).
		resp.IntegrationsPending = len(targets) > 0 && !r.integrationsWritten.Covers(targets)
	}
	r.mu.Unlock()

	// Record of today's behaviour (waired-agent#778), not a rule: the three
	// engine fields below are populated ONLY inside this branch, so a host
	// with no desired engine reports engine_installed=false however much
	// engine it has. A converged rc9 host serving on ollama answers
	// `{"active":false,"engine_installed":false,...}` here. Nothing is
	// mis-served by that today — every consumer gates on setupDriving()
	// (cmd/waired/setup_install.go) before reading them, so the fields are
	// only ever read in the window where they are populated. It is written
	// down because the field NAMES are unconditional while their meaning is
	// not, and the next reader comparing this against /inference/status will
	// otherwise see two surfaces contradicting each other on one host.
	if d.engine != "" {
		resp.EngineInstalled, resp.EngineReady = r.provider.setupEngineState(ctx, d.engine)
		// Only meaningful alongside EngineInstalled: "the files are there but
		// the thing will not run" is precisely the case the executor's
		// presence gate used to swallow (#330).
		//
		// Also only ever true for ollama: setupEngineHealth returns
		// (false, "") for every other engine kind, so a vLLM host cannot
		// reach the repair arm at all.
		if resp.EngineInstalled {
			resp.EngineNeedsRepair, _ = r.provider.setupEngineHealth(ctx, d.engine)
		}
	}
	// Published unconditionally. #115 served this only alongside a desired
	// engine, reasoning that there is nothing to install otherwise — that
	// turned out to be false. `waired init` on the daemon path installs
	// the engine whenever the host wants inference, wizard or not, and it
	// needs the destination in exactly that case (waired#835 §11).
	resp.StateDir = r.provider.setupStateDir()
	if d.modelID != "" {
		// Deliberately NOT the model_pull ROW the snapshot projects: that
		// row carries presentation rules this caller must not inherit —
		// #307 shows a failure the engine install can explain as `pending`
		// so exactly one row on the wizard is the live one. A local caller
		// wants the facts, and the one rule worth keeping is the
		// precedence: a recorded refusal outranks the lifecycle, because
		// the lifecycle for a model that was never admitted is
		// `not_present`, which on its own reads as "not started yet".
		resp.ModelState, _, _, _ = r.provider.setupModelState(d.modelID)
		resp.ModelErrorCode = rejected.code
		resp.ModelErrorDetail = clampSetupDetail(rejected.detail)
	}
	return resp
}

// snapshot builds the current typed progress (§7), or nil when this
// host has no onboarding activity. Statuses derive from observable
// state only, so a restarted agent reports the same truth.
// applyDesiredInference acts on the wizard's explicit local-AI answer
// (#597) — once per persisted VALUE, never per frame and never per
// process. The CP re-sends the instruction on every map frame and never
// clears it, so any weaker guard re-applies a weeks-old answer over a
// person's later local `waired inference off|on`: acting on the toggle
// state would do it every frame, an in-memory marker every restart. The
// durable record (state.SetupInference) is what lets a person's local
// flip stand until the wizard actually says something different — the
// #465 rule that an opt-in silently reverted on the next boot is no
// opt-in at all.
//
// A value outside the closed on/off set is left un-acted and UNRECORDED:
// a newer CP speaking a vocabulary this build does not know should find
// the instruction still pending after an upgrade, not consumed.
func (r *setupReconciler) applyDesiredInference(value string) {
	if value != signer.DesiredInferenceOn && value != signer.DesiredInferenceOff {
		return
	}
	r.mu.Lock()
	if r.inferenceActed.Value == value {
		r.mu.Unlock()
		return
	}
	rec := state.SetupInference{Value: value, AppliedAt: r.now().UTC().Format(time.RFC3339)}
	r.inferenceActed = rec
	r.mu.Unlock()
	switch value {
	case signer.DesiredInferenceOff:
		r.provider.setupDisableLocalInference("setup: the wizard asked this device to run without local inference")
	case signer.DesiredInferenceOn:
		r.provider.setupEnableLocalInference("setup: the wizard turned local inference back on")
	}
	if r.stateDir == "" {
		return
	}
	if err := state.WriteSetupInference(r.stateDir, rec); err != nil && r.logger != nil {
		r.logger.Warn("setup: could not persist the local-inference answer record; a restart may re-apply the instruction",
			"value", value, "err", err)
	}
}

// observedSetup names the engine, model and coding tools to report on for
// a host the control plane has never sent an instruction to
// (waired-agent#646) — the terminal-driven `waired init`, which writes its
// answers to this daemon and has no route to the desired columns.
//
// Reports false when there is nothing to say, which keeps two populations
// silent exactly as they were before: a device that has only enrolled, and
// one whose engine never arrived. Both would otherwise open a red
// engine_install row for an install nobody asked for.
//
// The pair is what makes the answer safe. setupServingEngine alone always
// names a kind, so it says nothing about this host; setupEngineState asks
// the disk, and the model is the one this device is set to serve, or —
// when nobody ever chose — the one it is actually serving. Together they
// mean "an engine is installed here and this device is running a model
// with it", which is the definition of a machine somebody set up.
//
// The model falls back for a reason. setupPreferredModelID reads
// preferred-model.json, which is a record of a CHOICE, and this function
// is a re-derivation of what the machine is DOING — the one gate of the
// three that was answering a different question. Hosts nobody asked never
// carry that record: `waired init --non-interactive` skips the picker
// outright, and on macOS the installer starts the daemon before init runs,
// so the bundled auto-pull is already under way when the picker looks and
// it steps aside for a host that has model history. Both left a machine
// that installed an engine, downloaded a model and answers requests
// publishing a document with zero steps — which the completion rule can
// never accept, so the console showed it as never having finished setting
// up and its model card, gated on that rule, stayed shut forever
// (waired-agent#753, #756).
//
// Preference FIRST, and the order is load-bearing: a choice that has not
// converged yet must still name its target, or the model row reports the
// finished download of the model the operator is switching away from.
//
// The #586 answers — "no model now" and an abandoned question — leave the
// preference empty too, and deliberately get no guard here. Both stand the
// bundled pre-pull down, so those hosts have no Ready model for the
// fallback to find; the only reachable combination is a host that answered
// "none" while something was already serving, and that machine IS serving
// it. Silence about a computer answering requests is the defect above, not
// a courtesy. (A host that deliberately runs with NO model still cannot
// satisfy a completion rule that needs at least one step. That is #586's
// own gap and wants its own terminal row, the way inference_off got one —
// there is no model here to name.)
//
// acted is the local-AI answer already acted on. An off-host reports its
// own `inference_off` row and has no engine to describe (#597), so
// synthesising engine and model rows beside it would contradict the row the
// completion rule reads.
//
// written is the coding-tool record (waired-agent#312), the only part of
// this that cannot be re-derived by looking at the machine. No record means
// no row — the same answer "nobody asked" already produces — rather than a
// claim about files the daemon deliberately never reads.
func (r *setupReconciler) observedSetup(ctx context.Context, acted string, written state.SetupIntegrations) (setupDesired, bool) {
	if acted == signer.DesiredInferenceOff {
		return setupDesired{}, false
	}
	engine := r.provider.setupServingEngine()
	if engine == "" {
		return setupDesired{}, false
	}
	if installed, _ := r.provider.setupEngineState(ctx, engine); !installed {
		return setupDesired{}, false
	}
	modelID := r.provider.setupPreferredModelID()
	if modelID == "" {
		modelID = r.provider.setupActiveModelID()
	}
	if modelID == "" {
		return setupDesired{}, false
	}
	return setupDesired{
		engine:  engine,
		modelID: modelID,
		// Already sorted and de-duplicated by the record's own reader, so
		// this is the same flat form flattenIntegrations produces. An empty
		// record yields "", which the projection reads as no instruction and
		// emits no row for.
		integrations: strings.Join(written.Targets, ","),
	}, true
}

// setupDriverFor names the surface that set this computer up, for
// SetupProgress.Driver.
//
// Strongest claim first. A live lease is a surface saying so about
// itself; the other two are derived, because neither surface holds a
// lease once its part is handed to the daemon:
//
//   - desired state is the browser's claim — the wizard wrote it, and
//     the write is the evidence (waired-agent#645);
//   - an observed setup is the terminal's — `waired init` writes its
//     answers to this daemon and nowhere else, so a host that can
//     describe itself with no instruction is one no browser ever drove.
//
// The two derivations cannot both apply: observedSetup only runs when
// there is no desired state at all.
//
// The terminal arm is what waired-agent#790 restores. The claim dies
// with `waired init`, and before #667/#771 the daemon then stopped
// pushing entirely, so the control plane kept the last document and the
// value looked durable. Once the observed projection started pushing on
// every tick, `Driver: ""` overwrote it — the column is replaced whole
// on each push — and only the browser half survived, because that half
// is re-derived rather than remembered.
//
// Derived rather than remembered on disk, for two reasons. A record
// could only be written by a future `waired init`, so every host already
// installed would stay wrong. And it would have to justify a push of its
// own to be read at all, which is exactly the zero-step document that
// pins the wizard on "waiting for this computer" (#198's card) for a
// machine with nothing left to wait for.
func setupDriverFor(claimed string, desiredActive, observed bool) string {
	switch {
	case claimed != "":
		return claimed
	case desiredActive:
		return signer.SetupDriverBrowser
	case observed:
		return signer.SetupDriverTerminal
	}
	return ""
}

func (r *setupReconciler) snapshot(ctx context.Context) *signer.SetupProgress {
	r.mu.Lock()
	d := r.desired
	active := r.active
	modelGenActed := r.modelGenActed
	rejected := r.modelRejected[d.modelID]
	leaseLive := r.leaseLiveLocked()
	everSeen := r.executorEverSeen
	elevated := r.executorElevated
	download, downloadSeen := r.executorSteps[setupStepEngineDownload]
	install := r.executorSteps[setupStepEngineInstall]
	integ, integSeen := r.executorSteps[setupStepIntegration]
	integWritten := r.integrationsWritten
	actedInference := r.inferenceActed.Value
	phase := install.phase
	execErr := install.errText
	// leaseLiveLocked above already dropped the claim if the lease died,
	// so reading it here needs no second liveness check. What it means
	// when there is none is setupDriverFor's question, below.
	claimed := r.executorDriver
	r.mu.Unlock()
	// A setup driven from the terminal leaves the control plane's desired
	// columns empty — only the management API writes them, so `waired init`
	// has no route to them at all — and everything above is derived from
	// those columns. The device therefore said nothing about itself, the
	// completion rule saw no steps, and the model card stayed shut on every
	// CLI-installed node (waired-agent#646).
	//
	// What the browser wrote, this host can be asked. Reporting the answer
	// is not a second opinion about the desired state: the rows below are
	// already re-derived from the disk and the engine probe on every
	// snapshot, and this only supplies the three values naming WHICH engine,
	// model and coding tools to report on.
	//
	// Deliberately only when the control plane is serving no instruction of
	// its own. An authored desired state is the operator's, and a derived
	// one racing it is how the two would come to disagree about the same
	// device.
	observed := false
	if !active {
		d, observed = r.observedSetup(ctx, actedInference, integWritten)
	}
	// A terminal takeover produces no desired state and therefore no
	// steps — but the wizard is on screen waiting for this device, and
	// with nothing pushed it waits forever. A driver alone is worth a
	// push: zero steps keeps setup_complete false and the "setup
	// unfinished" banner away, and tells the wizard who has it
	// (waired-agent#198).
	if !active && !observed && claimed == "" {
		return nil
	}

	p := &signer.SetupProgress{
		LastCheck: r.now().UTC().Format(time.RFC3339Nano),
		Driver:    setupDriverFor(claimed, active, observed),
		// The generation this report answers (#136). Without it a wizard
		// that has just bumped cannot tell "not picked up yet" from
		// "picked up, tried again, failed again" — the step is `failed`
		// in both, and those two want opposite things on screen.
		ModelGen: modelGenActed,
	}
	// The acted-on "don't run local AI on this computer" answer (#597):
	// the row the CP's completion derivation reads to count an off-host
	// as COMPLETE with no engine or model rows at all (the waired#835 §6
	// pair-contract amendment, waired#1109). Emitted from the ACTED
	// record, not the desired frame, so the wizard sees confirmation
	// rather than its own instruction read back.
	if actedInference == signer.DesiredInferenceOff {
		p.Steps = append(p.Steps, signer.SetupStep{ID: setupStepInferenceOff, Status: signer.SetupStatusDone})
	}
	if d.engine != "" {
		installed, ready := r.provider.setupEngineState(ctx, d.engine)
		// This probe is the OTHER reader of engine presence, and on a
		// 2 s ticker it is usually the first to see the binary land.
		// Apply only runs when a control-plane frame arrives, and nothing
		// local schedules one, so leaving the edge to Apply meant the
		// rows below could move ahead of the admission that produced them
		// (#413). Whichever probe gets there first owns the edge; the
		// latch makes sure only one does.
		if r.noteEngineInstalled(installed, d.modelID) {
			r.onEngineAppeared(d.engine, d.modelID)
			// rejected was read at the top of this function, from the
			// very record the edge just cleared. Re-read it so THIS
			// projection is already right rather than right on the next
			// tick — the stale row is the whole complaint.
			r.mu.Lock()
			rejected = r.modelRejected[d.modelID]
			r.mu.Unlock()
		}
		var engineLatched bool
		var engineLastErr string
		if installed && !ready {
			// Asked only when it can change the answer: a ready engine is
			// already Done, and an absent one has nothing to latch about.
			engineLatched, engineLastErr = r.provider.setupEngineHealth(ctx, d.engine)
		}
		// engine_download exists only when this host actually downloaded
		// something. A machine that already had the engine, or one whose
		// executor is older than the split, reports the single
		// engine_install row it always did — inventing a download row for
		// a transfer that never happened would be a step the wizard waits
		// on forever.
		if downloadSeen {
			p.Steps = append(p.Steps, engineDownloadStep(download, installed || ready, leaseLive))
		}
		step := signer.SetupStep{ID: setupStepEngineInstall}
		// Arm order is the contract here, not an accident (#187). It is
		// strongest-evidence-first: an engine that is serving beats a
		// stale failed phase from an earlier attempt; a failure the
		// executor reported beats mere on-disk presence, because a
		// half-configured install leaves a binary behind and still failed.
		switch {
		case ready:
			step.Status = signer.SetupStatusDone
		case phase == management.SetupExecutorPhaseFailed:
			// The executor tried and told us why. Its own text beats any
			// guess we could make from here. Ahead of `installed` so a
			// half-configured engine — a binary on disk that cannot
			// serve — reports the real failure instead of sitting at
			// "working on it" forever.
			step.Status = signer.SetupStatusFailed
			step.ErrorCode = executorErrorCode(install, classifySetupFailure)
			step.ErrorDetail = clampSetupDetail(execErr)
		case installed && engineLatched:
			// Installed, and the daemon has given up starting it. Ahead of
			// `installed` because that arm asks only "are the files there",
			// which a macOS bundle with a broken signature answers yes to
			// while every exec of it is killed — so the wizard reported "OK"
			// on every rerun over an engine that could never run (#330).
			//
			// Behind the executor's own failure: an executor that just tried
			// has fresher, more specific evidence than a latch from an
			// earlier attempt. Behind `ready` too — a serving engine is proof
			// the latch is stale.
			//
			// engine_not_ready, not the catch-all: nothing here is a network
			// or disk problem, and the engine's own last_error is the detail.
			step.Status = signer.SetupStatusFailed
			step.ErrorCode = signer.SetupErrorEngineNotReady
			step.ErrorDetail = clampSetupDetail(engineLastErr)
		case installed:
			// This step is "install the engine", and the engine is
			// installed. It used to complete only once the engine was
			// READY, which in this projection means the active MODEL is
			// ready — so the row said "working on it" for the whole model
			// download while the progress bar tracked the model step, and
			// the wizard showed two rows running at once.
			step.Status = signer.SetupStatusDone
		case downloadSeen && download.phase == management.SetupExecutorPhaseFailed:
			// The bytes never arrived, so there is nothing to install.
			// The failure is reported once, on the row it happened on;
			// repeating it here would show the operator two red steps for
			// one event and invite them to fix it twice.
			step.Status = signer.SetupStatusPending
		case downloadSeen && download.phase != management.SetupExecutorPhaseDone:
			// A download is still in flight. Keeping this row pending is
			// the same rule #187 settled for the model pull: exactly one
			// row is allowed to be the live one, or the wizard shows two
			// spinners and the progress bar belongs to neither.
			step.Status = signer.SetupStatusPending
		case phase == management.SetupExecutorPhaseDone:
			// The executor says it finished and we cannot see it yet.
			// Rare now that detection matches the daemon's own rule
			// (#179), but its explicit completion exists precisely to
			// advance the wizard, and discarding it is what left the step
			// spinning. The model step stays honest either way: its pull
			// is admitted from the engine probe, not from this.
			step.Status = signer.SetupStatusDone
		case leaseLive && !elevated:
			// An executor is present but cannot install — reporting
			// executor_gone here would send the operator to re-run a
			// command that would fail the same way.
			step.Status = signer.SetupStatusFailed
			step.ErrorCode = signer.SetupErrorPermissionDenied
		case leaseLive:
			// Elevated executor attached: installing, or about to.
			step.Status = signer.SetupStatusRunning
		case everSeen && !elevated:
			// It was here, it is gone, and it could not have installed
			// anything while it was here (waired-agent#137). Ahead of the
			// plain `everSeen` arm because that one says "run the command
			// again", and running the same unprivileged command again
			// fails the same way — the operator loops without ever being
			// told that privileges are the problem. executorElevated
			// outlives the lease precisely so this arm can be reached.
			step.Status = signer.SetupStatusFailed
			step.ErrorCode = signer.SetupErrorPermissionDenied
		case everSeen:
			// §9-4: it was here and it is gone. This is the recoverable
			// case — NAVI offers the command to re-run.
			step.Status = signer.SetupStatusFailed
			step.ErrorCode = signer.SetupErrorExecutorGone
		default:
			// Never attached at all this session, so nobody has run the
			// setup command here — which is what setup_command_not_run
			// says, and the code exists because that is a different thing
			// to tell the operator than a privileges problem
			// (waired-agent#312, on the coding-tools row).
			//
			// It used to report permission_denied on the grounds that an
			// unprivileged install is impossible. True, but it answers a
			// question nobody asked: nothing was refused here, and this row
			// ALSO reports permission_denied when an executor really was
			// refused (classifySetupFailure, above), so the two were
			// indistinguishable to the reader. Same defect #312 fixed one
			// row down; it was simply never carried over to this one.
			step.Status = signer.SetupStatusFailed
			step.ErrorCode = signer.SetupErrorSetupCommandNotRun
		}
		p.Steps = append(p.Steps, step)
		// The install-time measurement, directly after the engine that
		// makes it possible and before anything the operator has to answer
		// (waired#1143). The wire order IS NAVI's render order, and this is
		// the order the work happens in: the measurement runs off the
		// engine bootstrap, so on the browser path it is already going
		// while the wizard is still asking which model to install.
		//
		// Guarded by the engine, NOT by d.modelID, for that reason — gating
		// it on a chosen model would emit the rows only after the window
		// they describe had closed. Emits nothing at all on a host with no
		// measurement under way and none stored; see hostSpeedSteps.
		p.Steps = append(p.Steps,
			hostSpeedSteps(r.provider.setupHostSpeedProgress(), r.provider.setupModelState)...)
	}
	// The coding tools sit between the engine and the model download
	// (waired-agent#311). The wire order IS the order NAVI renders, and
	// this is the order the executor now works in: everything that needs
	// the operator's terminal — and their administrator rights — is done
	// before the multi-gigabyte transfer nobody has to watch.
	//
	// It used to be last, which put the one interactive step behind the
	// longest unattended wait of the whole flow: people walked away during
	// the download and came back to a wizard blocked on coding tools.
	// `|| integSeen` is waired-agent#791. An instruction or a persisted
	// success is not the only way this row can have an author: a terminal
	// apply that FAILED has one too, and neither of the other two ever
	// arrives for it — the record is written on the `done` edge alone.
	// Without this the failure had no row to land on and vanished, so the
	// completion rule never saw the step it should have been held open by.
	//
	// integrationStep reads the reported phase before any of its liveness
	// arms, so an empty instruction here cannot fall through to the
	// "nobody has run the setup command" arm.
	if d.integrations != "" || integSeen {
		p.Steps = append(p.Steps, integrationStep(d.integrations, integ, integrationWriter{
			leaseLive: leaseLive,
			everSeen:  everSeen,
			// Read from the rows already projected above, not from the raw
			// phases they came from: engine_download terminates itself on a
			// dead lease (#256) without its stored phase ever changing, and
			// the row on screen is what "already reported" has to mean.
			engineFailed: engineRowFailed(p.Steps),
			written:      integWritten,
		}))
	}
	if d.modelID != "" {
		step := signer.SetupStep{ID: setupStepModelPull}
		state, completed, total, errText := r.provider.setupModelState(d.modelID)
		switch {
		case rejected.detail != "":
			step.Status = signer.SetupStatusFailed
			step.ErrorCode = rejected.code
			step.ErrorDetail = clampSetupDetail(rejected.detail)
		case state == catalog.ModelStateReady:
			step.Status = signer.SetupStatusDone
		case state == catalog.ModelStateQueued || state == catalog.ModelStateDownloading || state == catalog.ModelStateVerifying:
			step.Status = signer.SetupStatusRunning
			step.CompletedBytes = completed
			step.TotalBytes = total
		case state == catalog.ModelStateFailed && engineRowBusy(p.Steps) &&
			engineInstallCouldExplain(classifyModelPullFailure(errText)):
			// #307: the engine is still being installed, so a failure this
			// row could not have avoided is not yet news. The rc7 hosts
			// carried a `failed` record from a pull attempted before the
			// engine existed, and projecting it turned the wizard's
			// "Download the AI model" row red — with "check its internet
			// connection" — while the engine's own progress bar was still
			// moving. Same rule the engine_install row already applies to
			// itself while a download is in flight: exactly one row is
			// allowed to be the live one.
			//
			// Read from the rows already projected above, for the reason
			// the integration row does.
			step.Status = signer.SetupStatusPending
		case state == catalog.ModelStateFailed:
			step.Status = signer.SetupStatusFailed
			step.ErrorCode = classifyModelPullFailure(errText)
			step.ErrorDetail = clampSetupDetail(errText)
		default: // not_present / evicted / unknown — pull not admitted yet
			step.Status = signer.SetupStatusPending
		}
		p.Steps = append(p.Steps, step)
	}
	if d.benchmarkGen > 0 {
		step := signer.SetupStep{ID: setupStepBenchmark}
		bs := r.provider.BenchmarkStatus()
		switch {
		case bs.Gen >= d.benchmarkGen && bs.State == management.BenchmarkStateDone:
			step.Status = signer.SetupStatusDone
			p.Benchmark = &signer.SetupBenchmark{
				Gen:           bs.Gen,
				MeasuredTokps: bs.MeasuredTokps,
				Trials:        bs.Trials,
				SpreadPct:     bs.SpreadPct,
				Method:        bs.Method,
			}
		case bs.Gen >= d.benchmarkGen && bs.State == management.BenchmarkStateFailed:
			step.Status = signer.SetupStatusFailed
			step.ErrorCode = benchmarkSetupErrorCode(bs.Outcome)
			step.ErrorDetail = clampSetupDetail(bs.Error)
			p.Benchmark = &signer.SetupBenchmark{Gen: bs.Gen}
		case bs.State == management.BenchmarkStateRunning:
			step.Status = signer.SetupStatusRunning
			// Per-measurement progress (waired-agent#199). MeasuredTokps
			// is deliberately absent while running: it is the FINAL
			// answer, and shipped wizards render it as "Speed: about N".
			// The converging figure goes in MedianTokps, and warm-up is
			// Trials set with Trial still 0.
			p.Benchmark = &signer.SetupBenchmark{
				Gen:         d.benchmarkGen,
				Trial:       bs.Trial,
				Trials:      bs.Trials,
				SampleTokps: bs.SampleTokps,
				MedianTokps: bs.MedianTokps,
				SpreadPct:   bs.SpreadPct,
				Method:      bs.Method,
			}
		default:
			step.Status = signer.SetupStatusPending
		}
		p.Steps = append(p.Steps, step)
	}
	return p
}

// benchmarkSetupErrorCode maps a benchmark run's ending to the code the
// wizard renders, instead of calling every ending an internal error.
//
// The enum already had the right value and this row never reached for
// it: an engine that was not ready is not a fault in Waired, it is a
// host to poll and retry, and the same distinction is drawn everywhere
// else in the benchmark path — RunBootBenchmark gates on it, the
// management API answers 425 rather than 503 for it, and
// runBenchmarkJob refuses to record it. Only this projection flattened
// it, so an operator whose engine had not finished installing was told
// "internal error" (waired-agent#203 proposal 1, on the surface a
// person actually reads).
//
// An unrecognised or empty outcome keeps the old code. Empty is a record
// written before Outcome was persisted, and guessing at it would be
// worse than the unspecific answer it already gives.
func benchmarkSetupErrorCode(outcome string) string {
	if outcome == benchOutcomeEngineNotReady {
		return signer.SetupErrorEngineNotReady
	}
	return signer.SetupErrorInternal
}

// engineRowFailed reports whether the rows the executor works through
// before it reaches the coding tools already carry a failure.
//
// model_pull and benchmark are deliberately not among them. Their
// failures have their own recovery — pick a different model, re-run the
// measurement — which is not the integration row's ("go back and run the
// setup command"), so those genuinely are two different things to tell
// the operator about.
func engineRowFailed(steps []signer.SetupStep) bool {
	for _, s := range steps {
		switch s.ID {
		case setupStepEngineDownload, setupStepEngineInstall:
			if s.Status == signer.SetupStatusFailed {
				return true
			}
		}
	}
	return false
}

// engineRowBusy reports whether an engine row is still working — the
// window in which a model failure cannot yet be the model's own fault.
//
// A FAILED engine row is deliberately not busy, and the exclusion is
// load-bearing rather than tidy. When the executor's engine download
// fails, engine_download goes red and engine_install is pinned at
// `pending` FOREVER by design (one event gets one red row). A bare
// pending-or-running test would therefore leave the model row grey for
// the rest of the process's life and setup_complete permanently out of
// reach — the never-resolving progress §9-4 exists to prevent, traded
// for the wrong-answer bug this is fixing.
//
// The test is "has not reached a terminal status", stated as the pair of
// non-terminal ones. Neither half is independently reachable today and
// that is fine, because the rule is what has to survive the arms above
// moving, not the current census: engine_download only ever projects
// done/failed/running, and every arm that leaves engine_install at
// `pending` requires a download row that is itself already running (busy
// by the other half) or failed (excluded by the check above). Mutation
// testing will call each half redundant; they are redundant with each
// other, not with nothing.
func engineRowBusy(steps []signer.SetupStep) bool {
	if engineRowFailed(steps) {
		return false
	}
	for _, s := range steps {
		switch s.ID {
		case setupStepEngineDownload, setupStepEngineInstall:
			switch s.Status {
			case signer.SetupStatusPending, signer.SetupStatusRunning:
				return true
			}
		}
	}
	return false
}

// engineInstallCouldExplain reports whether an engine that is still
// being installed is a plausible cause of a model failure with this
// code — i.e. whether finishing the install might make it go away.
//
// Three answers qualify, and `internal` is here because waired-agent#328
// moved the unattributable bucket into it. The text this gate was built
// for is `exit status 1` — a pull that died with nothing to say — which
// reached network_error only because that used to be the catch-all. Now
// that the classifier says "something went wrong" instead of blaming the
// internet, the same failure arrives as `internal`, and leaving it out
// would silently un-fix #307: the model row would go red again while the
// engine's own progress bar is still moving.
//
// A full disk, a model id that does not exist, and a timeout are NOT
// fixed by the engine arriving. The disk in particular: the window this
// gate covers — a multi-gigabyte model landing alongside a 1.4 GB engine
// — is the single most likely moment for one to fill, and hiding that
// for the length of the install would cost the operator the one thing
// they could have acted on.
func engineInstallCouldExplain(code string) bool {
	return code == signer.SetupErrorNetworkError ||
		code == signer.SetupErrorEngineNotReady ||
		code == signer.SetupErrorInternal
}

// integrationWriter is what snapshot() knows about the only party that
// can write the coding-tool row: the elevated executor. The daemon
// deliberately cannot — two of the three targets write into the invoking
// user's home and the third is root-owned managed settings — so with no
// executor this row has no author at all, and saying so is the whole of
// waired-agent#258.
type integrationWriter struct {
	leaseLive    bool // one is attached right now
	everSeen     bool // one was attached at some point this session
	engineFailed bool // an engine row already reports a failure
	// written is the instruction a past executor recorded as done, from
	// the daemon state dir (waired-agent#312). It outlives the lease, the
	// process and the reboot, which is the whole point: everything else
	// in this struct is a fact about THIS process's memory.
	written state.SetupIntegrations
}

// integrationStep projects the coding-agent instruction onto its §7 row
// (waired#935).
//
// "Asked, and every toggle was off" reports `skipped` rather than no row
// at all. §7 defines skipped as "already true on this computer", which is
// exactly what an all-off answer means — and reporting it is what lets
// the control plane tell an integration that was declined from one that
// was never asked about. Until now nothing in the agent produced
// `skipped`; this is its first producer.
func integrationStep(flat string, st setupExecutorStep, w integrationWriter) signer.SetupStep {
	step := signer.SetupStep{ID: setupStepIntegration}
	if flat == integrationsNone {
		step.Status = signer.SetupStatusSkipped
		return step
	}
	switch st.phase {
	case management.SetupExecutorPhaseDone:
		step.Status = signer.SetupStatusDone
	case management.SetupExecutorPhaseFailed:
		step.Status = signer.SetupStatusFailed
		step.ErrorCode = executorErrorCode(st, classifyIntegrationFailure)
		step.ErrorDetail = clampSetupDetail(st.errText)
	case management.SetupExecutorPhaseInstalling:
		step.Status = signer.SetupStatusRunning
	// No executor has spoken for this row (idle, or no report at all).
	// Which arm below applies is the difference between "wait" and
	// "nobody is coming": this used to be one unconditional `pending`, so
	// a wizard whose setup command had been closed showed a grey
	// coding-tools row for the rest of time, and setup_complete could
	// never become true (waired-agent#258).
	default:
		switch {
		case w.written.Covers(integrationTargets(flat)):
			// An executor wrote these, on some earlier run, and said so
			// (waired-agent#312). FIRST, ahead of every liveness arm below,
			// because none of them is asking the right question: they ask
			// who is attached to this daemon right now, and the coding
			// tools are files on a disk that outlive every lease. Without
			// this arm a completed device fell through to the last one on
			// every service restart and reported its finished row failed.
			step.Status = signer.SetupStatusDone
		case w.leaseLive:
			// An executor is here and has not reached this row yet. The
			// coding tools now come BEFORE the model download — they are
			// the last thing needing this terminal, and waired-agent#311
			// front-loaded them so the long unattended transfer is the tail
			// — so pending is a wait, and a short one, not a stall.
			step.Status = signer.SetupStatusPending
		case w.engineFailed:
			// The failure is already on screen, on the row it happened on.
			// Same rule as `downloadSeen && download.phase == failed`
			// keeping engine_install pending: one event gets one red row,
			// or the operator is invited to fix it twice. Re-running the
			// setup command is the recovery for both, and it resumes here.
			step.Status = signer.SetupStatusPending
		case w.everSeen:
			// §9-4: it was here and it is gone, before it got to this row.
			step.Status = signer.SetupStatusFailed
			step.ErrorCode = signer.SetupErrorExecutorGone
		default:
			// Never attached at all, and no record of a past one — the
			// browser-only host waired#935 left undecided. The daemon must
			// not become a privilege bridge into a user's home, so nothing
			// here can write these files; the setup command is the only
			// party that can, and it has not run.
			//
			// The code says exactly that (waired-agent#312). It used to be
			// permission_denied, which NAVI answers with "needs
			// administrator access to continue" — the wrong fact about a
			// device whose only omission was that nobody had run the
			// command, and the wrong one to leave on a row that ALSO
			// reports permission_denied when an executor really was
			// refused (see classifyIntegrationFailure). The recovery is the
			// same elevated command either way; the sentence is not.
			step.Status = signer.SetupStatusFailed
			step.ErrorCode = signer.SetupErrorSetupCommandNotRun
		}
	}
	return step
}

// classifyIntegrationFailure maps an integration failure to the §7 enum.
// Unlike an engine install these never fail for disk or network reasons
// — they write small files into a home directory — so the honest default
// is `internal`, and the detail carries the real text.
func classifyIntegrationFailure(errText string) string {
	if strings.Contains(strings.ToLower(errText), "permission denied") {
		return signer.SetupErrorPermissionDenied
	}
	return signer.SetupErrorInternal
}

// engineDownloadStep projects the executor's engine-download report onto
// its §7 row (#197). enginePresent short-circuits it: whatever the last
// lease said, an engine that is on this host was downloaded.
//
// The byte counters ride the row while it runs and are dropped once it is
// done — a finished download is a finished download, and a "1.4 GB /
// 1.4 GB" bar left on screen next to a green check is the kind of detail
// that reads as an unfinished step.
//
// leaseLive is what terminates the row when nobody is downloading any
// more (#256). This row only exists because a lease reported bytes
// against it, so a dead lease and a non-terminal phase can only mean the
// executor left mid-transfer: without this the bar sat at 40% forever,
// which is the never-resolving progress §9-4 exists to forbid.
func engineDownloadStep(st setupExecutorStep, enginePresent, leaseLive bool) signer.SetupStep {
	step := signer.SetupStep{ID: setupStepEngineDownload}
	switch {
	case enginePresent || st.phase == management.SetupExecutorPhaseDone:
		step.Status = signer.SetupStatusDone
	case st.phase == management.SetupExecutorPhaseFailed:
		step.Status = signer.SetupStatusFailed
		step.ErrorCode = executorErrorCode(st, classifySetupFailure)
		step.ErrorDetail = clampSetupDetail(st.errText)
	case !leaseLive:
		// The bytes stopped arriving because the process fetching them is
		// gone (Ctrl-C, a closed terminal, a crash). Recoverable, and the
		// same recovery as the install proper: run the command again.
		step.Status = signer.SetupStatusFailed
		step.ErrorCode = signer.SetupErrorExecutorGone
	default:
		step.Status = signer.SetupStatusRunning
		step.CompletedBytes = st.completedBytes
		step.TotalBytes = st.totalBytes
		step.RateBps = st.rateBps
	}
	return step
}

// setupDetailMax mirrors the control plane's error_detail clamp
// (waired#835 §20.5). Clamping here too keeps a long installer log from
// costing a whole push.
const setupDetailMax = 512

func clampSetupDetail(s string) string {
	return clampUTF8(s, setupDetailMax)
}

// clampUTF8 truncates s to at most max BYTES without splitting a rune.
//
// The budget is in bytes because the control plane's clamp is, but the
// cut has to land on a boundary: since #307 this text routinely carries
// the engine's own stderr, which means block-drawing progress glyphs and
// non-ASCII usernames out of Windows paths. A mid-rune cut is not an
// error anywhere — encoding/json substitutes U+FFFD — so it would have
// shown up only as mojibake in the wizard.
func clampUTF8(s string, max int) string {
	if len(s) <= max {
		return s
	}
	for max > 0 && !utf8.RuneStart(s[max]) {
		max--
	}
	return s[:max]
}

// diskFullMarkers are the substrings that mean "out of disk" across the
// three OSes and both engines' downloaders. Matching is best-effort by
// nature — the failure arrives as text — but the cost of guessing wrong
// is asymmetric: telling someone to check their internet connection when
// the real problem is a full disk sends them nowhere, while the reverse
// at least points at the machine.
var diskFullMarkers = []string{
	"no space left on device",
	"not enough space",
	"insufficient disk space",
	"insufficient space",
	"disk full",
	"enospc",
	"there is not enough space on the disk",
}

// interruptedMarkers name a failure THIS machine caused: a cancelled
// context, a killed child, an engine that could not be started for the
// download to talk to. None of them is a statement about the internet,
// and the pull-pipeline races (#305) produced them by the dozen — which
// is how the rc7 host ended up being told to check a working connection.
//
// `start ollama:` is the daemon's own wrapper around "there was no engine
// to pull with", so it is a marker rather than a substring accident.
var interruptedMarkers = []string{
	"context canceled",
	"context cancelled",
	"signal: killed",
	"signal: terminated",
	"signal: interrupt",
	"start ollama:",
	"start engine:",
}

// timeoutMarkers name a failure that ran out of time rather than out of
// connectivity. Kept separate from the network set because the enum
// already carries the distinction and the copy differs: "this took too
// long and was stopped" is actionable on a slow link, "check your
// internet connection" is not.
var timeoutMarkers = []string{
	"context deadline exceeded",
	"i/o timeout",
	"timed out",
	"timeout",
}

// networkMarkers are what a GENUINE network failure looks like. This set
// is now the only way to reach network_error from text — everything
// unrecognised is `internal`, so this list has to name the real ones
// rather than catch the leftovers.
var networkMarkers = []string{
	"no such host",
	"temporary failure in name resolution",
	"name resolution",
	"connection refused",
	// The Windows phrasing of the same thing, and it has to be here
	// explicitly: it contains none of the other markers, so without it an
	// engine download refused by a CDN or proxy would fall through to the
	// generic arm on the one OS whose error strings are prose. Same list
	// engineUnreachableMarkers keys on, read for the opposite purpose —
	// there it means the LOCAL engine, on the one row that can safely
	// assume that (#307).
	"actively refused it",
	"connection reset",
	"network is unreachable",
	"no route to host",
	"broken pipe",
	"unexpected eof",
	"tls",
	"certificate",
	"proxy",
	"dial tcp",
	"dial udp",
	"bad gateway",
	"service unavailable",
}

// loopbackMarkers name this machine in a dialled address.
var loopbackMarkers = []string{
	"127.0.0.1",
	"localhost",
	"[::1]",
}

// setupModelRejection is why applying the desired model was refused: the
// §7 code and the text the operator sees under it.
//
// The pair is stored rather than the text alone because the code cannot
// be recovered from the text afterwards. Every refusal used to be
// reported as model_not_found, whose recovery is "pick a different
// model" — so an operator whose engine was simply too old for their
// choice tried model after model, and a host with pulls turned off got
// the same advice with nothing that could ever satisfy it
// (waired-agent#134).
type setupModelRejection struct {
	code   string
	detail string
}

// classifyModelRejection maps a refusal from setupApplyModel to the §7
// enum, by SENTINEL rather than by prose: the errors are produced in
// this same package (PullModel, SwapPreferredModel), so the value is
// still here to inspect and there is no reason to guess from a string
// the way the cross-process classifiers above have to.
//
// The default stays model_not_found — an unknown alias and a manifest
// with no variants really are "that model is not available", and that is
// what this path reported for everything before.
func classifyModelRejection(err error) string {
	switch {
	case errors.Is(err, errEngineTooOld), errors.Is(err, errEngineNotInstalled):
		// The model exists and this host simply cannot load it yet —
		// either the engine is too old for it, or (#307) there is no
		// engine binary at all. Telling the operator to choose another
		// model is not wrong, but it is not the fix either: this is the
		// enum's engine-side code, and the engine row already carries the
		// action that resolves both.
		return signer.SetupErrorEngineNotReady
	case errors.Is(err, errPullsDisabled), errors.Is(err, errUnsupportedSource):
		// Configuration and host-shape refusals. Nothing about the model
		// is wrong and changing it changes nothing, so model_not_found
		// would send the operator round a loop; `internal` says "this
		// computer will not do it" and the detail says why.
		return signer.SetupErrorInternal
	case errors.Is(err, management.ErrModelSwitchUnavailable):
		// The download could not be STARTED, and the two cases above did
		// not claim it — so this is the state store failing to record the
		// job, whose text is the only evidence there is. Reading it is
		// what the cross-process classifier is for, and it is the one
		// place a full disk shows up on this path. Last, so the specific
		// sentinels the swap wraps still win.
		return classifySetupFailure(err.Error())
	}
	return signer.SetupErrorModelNotFound
}

// classifySetupFailure maps a free-form failure string to the §7 error
// code enum.
//
// The default is `internal`, and that is the whole of #328. It used to be
// network_error, so a download killed by this machine — `exit status 1`,
// `download: start ollama: context canceled` — reached the wizard as
// "could not finish downloading. Check its internet connection." In the
// rc7 review that sent the operator to look at a connection that was
// fine, while the daemon's journal held the real cause the whole time.
//
// Guessing from text is unavoidable here: the failure crosses a process
// boundary as prose (which is exactly why the executor's DECLARED code
// wins where there is one — see executorErrorCode). What is avoidable is
// guessing CONFIDENTLY. "Something went wrong on this computer", with the
// reason underneath it, is a true statement about an unrecognised
// failure; blaming the network is a specific claim that is usually wrong.
//
// Order matters and is pinned by tests:
//
//   - disk first: it is the most likely way a multi-GB download dies and
//     the one whose wrong answer wastes the most of the operator's time.
//   - interrupted before everything: a `connection refused` to this
//     machine's own engine is not a network problem, and the text says
//     so only by naming a loopback address.
//   - timeout before network: `i/o timeout` is both, and "this took too
//     long" is the more actionable half.
func classifySetupFailure(errText string) string {
	if isDiskFullText(errText) {
		return signer.SetupErrorDiskFull
	}
	l := strings.ToLower(errText)
	if isLocalRefusalText(l) || containsAnyMarker(l, interruptedMarkers) {
		return signer.SetupErrorInternal
	}
	if containsAnyMarker(l, timeoutMarkers) {
		return signer.SetupErrorTimeout
	}
	if containsAnyMarker(l, networkMarkers) {
		return signer.SetupErrorNetworkError
	}
	return signer.SetupErrorInternal
}

// classifyModelPullFailure classifies a recorded MODEL PULL failure.
//
// Separate from classifySetupFailure, which it falls through to, and the
// separation is the point: that one is shared with both engine rows via
// executorErrorCode, so teaching it to read connect-refused wording
// would relabel a genuine engine DOWNLOAD failure — a CDN or proxy that
// refused the connection — as engine_not_ready, inverting this fix on
// the row next door. Only the model row can safely read a refused
// connection as "the local engine is not there": it is the only step
// whose work is done by a client of that engine.
//
// Order is a contract. A full disk wins over everything, because both
// markers genuinely co-occur — a pull that could not reach the engine
// and could not have written the bytes anyway — and of the two, only the
// disk is a thing the operator must act on.
func classifyModelPullFailure(errText string) string {
	if isDiskFullText(errText) {
		return signer.SetupErrorDiskFull
	}
	if isEngineUnreachableText(errText) {
		return signer.SetupErrorEngineNotReady
	}
	return classifySetupFailure(errText)
}

// engineUnreachableMarkers name a LOCAL engine that could not be reached,
// never a remote transfer that failed.
//
// The first entry is ours, written by runPullJob when EnsureRunning fails
// on the attempt that then failed; it is the only one that carries a
// reason with it. The rest are what surfaces when the engine dies between
// the readiness check and the download, so nothing on our side saw it —
// the ollama CLI's own wording, and the two shapes a refused TCP connect
// takes. "actively refused it" is the Windows phrasing, matching the list
// cmd/waired/main.go's isConnectionRefused already keys on.
//
// Deliberately NOT "connection reset by peer" or a bare "connection":
// a reset is a transfer that started and died, which is the network's
// problem and is already classified as one.
var engineUnreachableMarkers = []string{
	engineNotRunningMarker,
	"could not connect to ollama",
	"connection refused",
	"actively refused it",
}

func isEngineUnreachableText(errText string) bool {
	l := strings.ToLower(errText)
	for _, m := range engineUnreachableMarkers {
		if strings.Contains(l, strings.ToLower(m)) {
			return true
		}
	}
	return false
}

// isDiskFullText reports whether a failure string names a full disk.
// Shared with the pull retry (#306), which must not spend three multi-GB
// attempts on a condition that cannot clear itself.
func isDiskFullText(errText string) bool {
	return containsAnyMarker(strings.ToLower(errText), diskFullMarkers)
}

// isLocalRefusalText reports whether a refused connection names THIS
// machine. l is already lowercased.
//
// Both halves are required. "connection refused" alone is an ordinary
// network failure when the peer is a registry; a loopback address alone
// appears in plenty of messages that are not refusals. Together they mean
// the engine on this host was not listening — a self-inflicted state with
// its own recovery, and never an internet problem.
func isLocalRefusalText(l string) bool {
	if !strings.Contains(l, "connection refused") {
		return false
	}
	return containsAnyMarker(l, loopbackMarkers)
}

func containsAnyMarker(lowered string, markers []string) bool {
	for _, m := range markers {
		if strings.Contains(lowered, m) {
			return true
		}
	}
	return false
}

// progressKey canonicalizes a snapshot for change detection, ignoring
// the always-moving LastCheck timestamp.
func progressKey(p *signer.SetupProgress) string {
	c := *p
	c.LastCheck = ""
	b, err := json.Marshal(c)
	if err != nil {
		return ""
	}
	return string(b)
}

// runPush is the reporter loop (§5.2/§7): every setupPushInterval it
// snapshots the step states and pushes to CP when the content changed
// since the last successful push, or when the last push has aged past
// setupKeepaliveInterval (#130 — an unchanging step must still prove it
// is alive). Hosts with no onboarding activity snapshot nil and never
// touch the network: the setup channel adds zero heartbeat to a fleet at
// rest, and the keepalive only runs for a host that is mid-setup.
func (r *setupReconciler) runPush(ctx context.Context) {
	if r == nil || r.push == nil || r.deviceID == "" || len(r.machineKey) != ed25519.PrivateKeySize {
		return
	}
	t := time.NewTicker(r.interval)
	defer t.Stop()
	var lastPushed string
	var lastPushAt time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-r.kick:
		}
		// Before the projection, so a model admitted on this tick is
		// already in the snapshot the same tick reports (the reason
		// snapshot() owns the engine edge too — see #413 there).
		//
		// Placed inside this loop rather than in its own goroutine: the
		// early return above means a daemon with no push client never
		// reconciles, and that is correct — no push client is no control
		// plane, so there is no desired state to converge on.
		r.reconcileDesiredModel(ctx)
		// And the benchmark the same tick, for the same reason: the pull
		// that finishing makes it runnable is an edge no frame reports.
		r.reconcileBenchmark()
		snap := r.snapshot(ctx)
		if snap == nil {
			continue
		}
		key := progressKey(snap)
		if key == "" {
			continue
		}
		if key == lastPushed && r.now().Sub(lastPushAt) < setupKeepaliveInterval {
			continue
		}
		pushCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		err := r.push.PushSetupProgress(pushCtx, r.deviceID, *snap, r.machineKey)
		cancel()
		if err != nil {
			if r.logger != nil && !errors.Is(err, context.Canceled) {
				r.logger.Warn("setup progress push failed", "err", err)
			}
			continue // retry with fresh content next tick
		}
		lastPushed = key
		lastPushAt = r.now()
	}
}

// --- agentInferenceProvider adapters (the real setupProvider) ---

// setupEngineState reports whether the desired engine kind is installed
// on this host and whether it is the one currently serving and ready.
// Installation state goes through engineInstalledOnHost — the daemon's
// own resolution rule — and readiness through the provider's usual
// EngineReady gate.
//
// It used to read the hardware profile's Engines map, which is a
// PATH-only probe: an engine installed under the state dir (the normal
// case, and the only one on Linux) read as absent, so /setup/state and
// /inference/status contradicted each other on the same host at the
// same instant. The wizard then never admitted the model pull, the step
// contents never changed, the progress push deduped everything away,
// last_check froze, and the run ended as executor_gone (#179).
//
// Resolving live also drops the profile's 30 s cache from the path, so
// an engine the executor just installed is visible on the next frame
// rather than up to half a minute later.
// The context parameter is kept for the setupProvider interface (a fake
// implements it too) but is no longer needed: nothing here profiles.
func (p *agentInferenceProvider) setupEngineState(_ context.Context, engine string) (installed, ready bool) {
	if !engineInstalledOnHost(runtime.GOOS, p.stateDir, engine) {
		return false, false
	}
	if p.servingEngine() != engine {
		return true, false
	}
	r, _ := p.EngineReady()
	return true, r
}

// setupEngineHealth reports a LATCHED engine failure and its reason.
//
// The truth was always one struct away from snapshot() — the very frame that
// reported engine_installed=true also carried subsystem_state="engine_failed"
// and the engine's own last_error — but the setup projection had no way to ask
// for it, so the wizard's engine row said OK over a dead engine forever (#330).
//
// The latch is set only after the recovery budget is spent (3 attempts inside
// a 5-minute stability window) — by onEngineUnhealthy for an engine that died
// while serving, and by onEngineStartFailed for one that never came up at all,
// which is the macOS case #330's arm was written for and could not reach
// (#310).
//
// The reason comes from FailureLatchedReason, not Health(): those two have
// different lifetimes, and reading the wrong one is how this returned
// (true, "") — a red row with nothing on it — after any Stop in between.
func (p *agentInferenceProvider) setupEngineHealth(_ context.Context, engine string) (bool, string) {
	// Only ollama runs under the adapter that latches; vLLM has no equivalent
	// give-up state yet, and claiming one would be a lie.
	if p.ollama == nil || engine != catalog.RuntimeOllama {
		return false, ""
	}
	// Not the engine we are actually serving: whatever it is doing is not
	// this step's business.
	if p.servingEngine() != engine {
		return false, ""
	}
	latched, reason := p.ollama.FailureLatchedReason()
	if !latched {
		return false, ""
	}
	return true, reason
}

// setupStateDir is the agent's state root. The executor installs the
// bundled engine relative to this, so it matches bundledOllamaBinPath's
// join (engine_resolve.go) by construction rather than by coincidence.
func (p *agentInferenceProvider) setupStateDir() string { return p.stateDir }

// setupServingEngine is the engine kind this process serves from. Same
// value setupEngineState compares against, so the two cannot disagree
// about which engine the observed projection is describing.
func (p *agentInferenceProvider) setupServingEngine() string { return p.servingEngine() }

// setupModelState reports one catalog model's lifecycle state, live
// pull bytes (while downloading) and the stored failure detail.
func (p *agentInferenceProvider) setupModelState(modelID string) (string, int64, int64, string) {
	st, err := p.store.Load()
	if err != nil {
		return "", 0, 0, ""
	}
	ms, ok := st.Models[modelID]
	if !ok {
		return catalog.ModelStateNotPresent, 0, 0, ""
	}
	completed, total, _ := p.dlProgress.aggregate(modelID)
	return ms.State, completed, total, ms.Error
}

// startSetupBenchmark kicks the single-flight benchmark job (#99) at
// the served generation without waiting for it.
func (p *agentInferenceProvider) startSetupBenchmark(gen int) {
	p.startBenchmarkJob(gen)
}

// startSetupEngine adopts an engine installed after this daemon booted
// (#304). Coalesced and dispatched on the daemon's own context by
// requestEngineStart; a parked or crash-latched engine is left alone.
// setupEnableLocalInference turns local inference on when the wizard's
// desired state arrives on a device that had it off (#465). A no-op when
// it is already on, so the per-frame reconcile costs nothing and the
// desired-inference file is not rewritten on a cadence.
//
// enableInference is nil on a daemon started with --disable-inference:
// there is no subsystem to turn on, and the operator's kill switch is
// not something a control-plane instruction may override.
func (p *agentInferenceProvider) setupEnableLocalInference(reason string) {
	if p == nil || p.enableInference == nil {
		return
	}
	if p.isInferenceDisabled == nil || !p.isInferenceDisabled() {
		return
	}
	if p.logger != nil {
		p.logger.Info("turning local inference on", "reason", reason)
	}
	if err := p.enableInference(); err != nil && p.logger != nil {
		p.logger.Warn("could not turn local inference on", "reason", reason, "err", err)
	}
}

// setupDisableLocalInference turns local inference off when the wizard's
// explicit answer arrives (#597; waired#1109/#1110) — the same persisted
// soft toggle a person's own `waired inference off` writes (#465), so a
// recorded choice, never a fault, and the #569 closing-box arms read it
// exactly like one. A no-op when it is already off, for the reason the
// enable above states.
//
// disableInference is nil on a daemon started with --disable-inference:
// the subsystem is already out, and there is nothing to persist through.
func (p *agentInferenceProvider) setupDisableLocalInference(reason string) {
	if p == nil || p.disableInference == nil {
		return
	}
	if p.isInferenceDisabled != nil && p.isInferenceDisabled() {
		return
	}
	if p.logger != nil {
		p.logger.Info("turning local inference off", "reason", reason)
	}
	if err := p.disableInference(); err != nil && p.logger != nil {
		p.logger.Warn("could not turn local inference off", "reason", reason, "err", err)
	}
}

func (p *agentInferenceProvider) startSetupEngine(reason string) {
	p.requestEngineStart(reason)
}

// setupPreferredModelID is the model this device is currently set to
// serve. It reads the EFFECTIVE preference (the in-process #812
// override when one has been published, else the boot snapshot), not
// the file, so a choice applied moments ago is already visible and the
// reconciler does not re-apply it on the next frame.
func (p *agentInferenceProvider) setupPreferredModelID() string {
	return p.effectivePreferredModelID()
}

// setupActiveModelID is the model this device is actually serving. Same
// value ActiveModelID publishes to the control plane as
// inference_detail.active_model, so the setup report and the inference
// report cannot describe the same host differently. See the interface for
// why the observed projection needs it beside the preference.
func (p *agentInferenceProvider) setupActiveModelID() string {
	id, _ := p.ActiveModelID()
	return id
}

// setupCanonicalModelID resolves a control-plane model name to the id
// this device keys its own state by. See the interface for why the
// convergence compare depends on it.
//
// It resolves against p.manifests, which is the COMPLETE set — this is
// resolution, not offering, and the control plane may legitimately desire
// a withheld model (the routing sentinel pins one).
func (p *agentInferenceProvider) setupCanonicalModelID(name string) string {
	return canonicalSetupModelID(name, p.manifests)
}

// canonicalSetupModelID is setupCanonicalModelID's whole body, as a free
// function over an injected catalog.
//
// Separate so the reconciler's fake provider runs the SAME resolution the
// daemon does instead of an identity stub: a fake that returned its
// argument unchanged would make the convergence bug this fixes
// unwritable as a test, which is the shape CLAUDE.md §Test discipline
// calls a defective fake.
func canonicalSetupModelID(name string, manifests []catalog.Manifest) string {
	if name == "" {
		return ""
	}
	if m, _, ok := catalog.ResolveModel(name, manifests); ok && m.ModelID != "" {
		return m.ModelID
	}
	return name
}

// setupApplyModel makes modelID the model this device serves. It is the
// setup path's half of the operator model switch, and mirrors
// management.handleInferencePreferredModel step for step:
//
//  1. Persist the preference, so the choice survives the restart an
//     engine install may cause. bootstrapPreferredModel re-pulls or
//     activates it on the next boot without any further instruction.
//  2. Apply it in process (#812): on-disk weights flip the active
//     selection and bounce the engine now; absent weights start a pull
//     whose completion runs activatePreferredIfNeeded.
//  3. Fall back to a bare pull when the in-process switch declines the
//     target (errSwapNeedsRestart — a cross-engine change). The weights
//     land now and step 1's preference activates them after the restart.
//
// Unlike the management handler this NEVER schedules a restart of its
// own: it runs while a browser wizard is watching the setup steps, and
// taking the daemon down mid-run is the class of silence #130 exists to
// prevent. The engine install the wizard is driving supplies whatever
// restart is genuinely needed.
//
// The switch runs on the daemon's long-lived context, never the frame's:
// Apply's context belongs to the network-map stream, and the pull plus
// engine bounce have to outlive it (same reason as modelSwapController).
func (p *agentInferenceProvider) setupApplyModel(ctx context.Context, modelID string) (bool, error) {
	if p.preferencePath != "" {
		// Source desired: this is the control plane's instruction arriving,
		// not an answer given here. The distinction is the whole of
		// waired-agent#627 (an arriving instruction used to delete the
		// install picker, because a preference existed) and half of
		// waired-agent#647 (an instruction must not be able to confirm
		// itself back to the control plane as a local choice).
		if err := agentconfig.SavePreference(p.preferencePath, agentconfig.Preference{
			ModelID: modelID,
			Source:  agentconfig.PreferenceSourceDesired,
		}); err != nil {
			// Not fatal: the in-process switch below still makes this the
			// served model for the life of this process. Only the
			// survives-a-restart guarantee is lost, and saying so beats
			// failing a setup the user can see working.
			p.logger.Warn("setup: persisting the chosen model failed", "model", modelID, "err", err)
		}
	}
	applyCtx := p.agentCtx
	if applyCtx == nil {
		applyCtx = ctx
	}
	// #496: measure the host before the weights start arriving.
	//
	// This is the OTHER install path — a person chose this model in the
	// browser — so the cutoff's decision is deliberately not taken here:
	// they have said they want to serve, and #465's default is not ours to
	// override. The measurement is taken all the same, because it is a
	// property of the host rather than a step of that decision, and
	// because this is the majority path: skipping it here would leave the
	// control plane, the device page and waired#1065 with a figure for
	// almost no one. Cached per engine build, so a later model switch on
	// the same host costs nothing.
	//
	// Before rather than after the download for the same reason the
	// bundled path measures early: a 20-45 GB transfer saturates the link
	// and the disk, and a measurement taken alongside one measures the
	// contention.
	if modelID != hostfit.HostCutoffProbeModelID {
		// The install window, not the background one: SwapPreferredModel is
		// on the next line and the weights start arriving from it. This path
		// does not even take the verdict — see above — so blocking a chosen
		// download on it for the full budget buys the operator nothing
		// (waired-agent#579).
		p.ensureHostSpeedMeasured(applyCtx, p.hostSpeedInstallWindow())
	}
	downloading, err := p.SwapPreferredModel(applyCtx, modelID)
	if err == nil {
		return downloading, nil
	}
	if !errors.Is(err, errSwapNeedsRestart) {
		return false, err
	}
	p.logger.Info("setup: model switch needs a restart; downloading now and activating on the next boot",
		"model", modelID)
	if _, perr := p.PullModel(applyCtx, modelID); perr != nil {
		return false, perr
	}
	return true, nil
}

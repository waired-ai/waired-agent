package main

import (
	"context"
	"log/slog"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"cloud.google.com/go/compute/metadata"
	"cloud.google.com/go/logging"

	"github.com/waired-ai/waired-agent/internal/management"
)

// statsPublishKickoff is the earliest record the agent emits to Cloud
// Logging — fired from runStatsPublisher BEFORE the ticker. It tells
// the testnet verify script "this agent is alive and its Cloud Logging
// path works", separating "agent slow to boot" from "agent publishing
// fails" failure modes during diagnostics.
const statsPublishKickoff = "waired_agent_kickoff"

// runStatsPublisher periodically emits a structured record summarising
// the agent's current health, intended for the testnet CI verify path
// (scripts/dev/testnet-punch-verify.sh) which reads back via
// `gcloud logging read`.
//
// Two sinks, in order:
//
//  1. Local slog at Info — always. On developer machines this is the
//     only visible record (stderr).
//  2. Cloud Logging via cloud.google.com/go/logging — only when the
//     binary detects it is running on GCE (compute metadata server
//     reachable). The earlier ops-agent-based design that scraped
//     stderr through systemd-journal didn't work on the testnet VMs
//     (the install script was unreliable, and likely unreachable
//     under the testnet VPC's egress posture). Calling the Cloud
//     Logging API directly side-steps the entire systemd / journald
//     plumbing — auth comes from the metadata server using the VM's
//     attached SA (test_agent_sa, which already has
//     roles/logging.logWriter).
//
// Lifetime is bounded by ctx; cancellation flushes the Cloud Logging
// client so buffered entries reach the API before exit.
//
// cloudLogSink, when non-nil, receives the lazy-initialised
// *cloudLogger (or nil on non-GCE) once newCloudLoggerOnGCE returns.
// The testharness dispatcher's Reporter consumes it through the same
// pointer so its scenario records share a single Cloud Logging client
// without the publisher and dispatcher having to coordinate startup
// order.
func runStatsPublisher(ctx context.Context, p management.StatusProvider, interval time.Duration, cloudLogSink *atomic.Pointer[cloudLogger]) {
	if interval <= 0 {
		interval = 5 * time.Second
	}
	cl := newCloudLoggerOnGCE(ctx)
	if cloudLogSink != nil {
		cloudLogSink.Store(cl)
	}
	if cl != nil {
		defer cl.close(ctx)
		// Fire one kickoff record immediately so verify can confirm
		// agent ↔ Cloud Logging works without waiting for the first
		// stats sample. Carries the same Status payload for free.
		cl.publishKickoff(p.Status())
	}
	// Fire the first stats sample immediately, then on every tick.
	// The previous behaviour (first sample only after the first tick)
	// added 5–10 s of needless latency to verify's "ready" signal.
	emitStatsRecord(p.Status(), cl)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			emitStatsRecord(p.Status(), cl)
			// Keep the last test-scenario state fresh in Cloud Logging so
			// the fallback runner's poll survives per-record ingest jitter
			// under load (#592). No-op until a scenario has been reported
			// (always, in production builds).
			cl.republishLastScenario()
		}
	}
}

// emitStatsRecord writes a single agent-stats sample to slog and (if
// configured) Cloud Logging. Broken out of runStatsPublisher so unit
// tests can drive it without spinning the ticker. cl == nil is the
// developer-machine path; the slog-only emit still runs.
func emitStatsRecord(st management.Status, cl *cloudLogger) {
	slog.Info("waired_agent_stats",
		"network_id", st.NetworkID,
		"device_id", st.DeviceID,
		"device_name", st.DeviceName,
		"overlay_ip", st.OverlayIP,
		"listen_port", st.ListenPort,
		"nat_type", st.NATType,
		"observed_addr", st.ObservedAddr,
		"observed_addr_v6", st.ObservedAddrV6,
		"first_observed_v6_unix", st.FirstObservedV6Unix,
		"stun_attempts_v4", st.STUNAttemptsV4,
		"stun_attempts_v6", st.STUNAttemptsV6,
		"stun_responses_v4", st.STUNResponsesV4,
		"stun_responses_v6", st.STUNResponsesV6,
		"disco_enabled", st.DiscoEnabled,
		"peer_count", st.PeerCount,
		"phase", st.Phase,
		"desired_phase", st.DesiredPhase,
		"peers", st.Peers,
	)
	if cl != nil {
		cl.publish(st)
	}
}

// statsIntervalFromEnv reads WAIRED_STATS_INTERVAL_S and returns the
// publisher's tick interval. Falls back to the runStatsPublisher default
// (10 s) when the env var is unset, empty, non-numeric, or non-positive.
func statsIntervalFromEnv() time.Duration {
	v := os.Getenv("WAIRED_STATS_INTERVAL_S")
	if v == "" {
		return 0
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return 0
	}
	return time.Duration(n) * time.Second
}

// cloudLogger is a tiny wrapper around cloud.google.com/go/logging that
// (a) hides project-id discovery, (b) renders a Status snapshot as a
// structured Cloud Logging Entry with the keys testnet-punch-verify.sh
// expects, and (c) lets emitStatsRecord stay agnostic about whether
// logging is actually wired (cl == nil ⇒ no-op).
type cloudLogger struct {
	client *logging.Client
	logger *logging.Logger

	// instanceName is the GCE VM name, surfaced as a Cloud Logging
	// label so verify can filter per-VM without depending on
	// resource.labels.instance_name (which is auto-attached when the
	// log entry is emitted via a writer auto-detected as MonitoredResource
	// type "gce_instance" — usually true on GCE, but the label is a
	// belt-and-braces fallback for the filter).
	instanceName string

	// lastScenario caches the most recent test-scenario state-change so
	// runStatsPublisher can re-publish it on every stats tick (#592).
	// nil until the first ReportScenario; in production (!testharness)
	// builds the NoopDispatcher never reports, so it stays nil and
	// republishLastScenario is a permanent no-op. Written from the
	// dispatcher goroutine, read from the stats-publisher goroutine —
	// atomic.Pointer makes that race-free.
	lastScenario atomic.Pointer[scenarioRecord]

	closed  bool
	closeMu sync.Mutex
}

// scenarioRecord is a cached snapshot of the last test-scenario
// state-change the agent reported. The single-shot publishScenario
// record is emitted once per apply/revert transition, which makes it
// brittle to per-record Cloud Logging ingest jitter under concurrent-PR
// testnet load (#592): if that one entry's ingestion lags,
// testnet-fallback-runner.sh's poll_scenario_state times out even though
// the periodic waired_agent_stats stream from the same VM ingests fine.
// Re-emitting this cached state on the stats ticker makes the scenario
// signal self-healing — a delayed entry is followed by a fresh copy
// within one tick, so it inherits the reliability of the
// waired_agent_stats poll.
type scenarioRecord struct {
	state        string
	scenarioID   string
	peerDeviceID string
	nonce        int64
	errMsg       string
}

// newCloudLoggerOnGCE returns a cloudLogger when the compute metadata
// server reachable (i.e. running on GCE) AND the project + instance
// name lookups succeed. On any failure (developer laptop, network
// hiccup, missing SA) it returns nil — the caller treats nil as the
// "skip Cloud Logging" sentinel.
func newCloudLoggerOnGCE(ctx context.Context) *cloudLogger {
	if !metadata.OnGCE() {
		return nil
	}
	probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	project, err := metadata.ProjectIDWithContext(probeCtx)
	if err != nil || project == "" {
		slog.Warn("cloud-logging: project lookup failed; falling back to slog-only", "err", err)
		return nil
	}
	instance, err := metadata.InstanceNameWithContext(probeCtx)
	if err != nil {
		instance = "" // best-effort — emit anyway
	}
	client, err := logging.NewClient(ctx, project)
	if err != nil {
		slog.Warn("cloud-logging: NewClient failed; falling back to slog-only", "err", err)
		return nil
	}
	cl := &cloudLogger{
		client:       client,
		logger:       client.Logger("waired-agent"),
		instanceName: instance,
	}
	slog.Info("cloud-logging: stats publisher enabled", "project", project, "instance", instance)
	return cl
}

// publishKickoff fires once at startup. Same payload shape as publish()
// but with msg="waired_agent_kickoff" so testnet-punch-verify (and
// debugging) can distinguish "agent successfully initialised the
// Cloud Logging client" from regular periodic samples.
func (c *cloudLogger) publishKickoff(st management.Status) {
	if c == nil || c.logger == nil {
		return
	}
	payload := buildPayload(statsPublishKickoff, st)
	c.logger.Log(logging.Entry{
		Severity: logging.Info,
		Payload:  payload,
		Labels: map[string]string{
			"event":         statsPublishKickoff,
			"instance_name": c.instanceName,
		},
	})
}

// publish writes a single Status snapshot as a structured Cloud
// Logging Entry. The payload mirrors emitStatsRecord's slog keys so
// the CI verify filter can use either source interchangeably.
func (c *cloudLogger) publish(st management.Status) {
	if c == nil || c.logger == nil {
		return
	}
	payload := buildPayload("waired_agent_stats", st)
	c.logger.Log(logging.Entry{
		Severity: logging.Info,
		Payload:  payload,
		Labels: map[string]string{
			"event":         "waired_agent_stats",
			"instance_name": c.instanceName,
		},
	})
}

// buildPayload renders a Status snapshot into the JSON payload shape
// shared by publish() and publishKickoff(). Keys mirror the slog
// attributes in emitStatsRecord().
func buildPayload(msg string, st management.Status) map[string]any {
	return map[string]any{
		"msg":                    msg,
		"network_id":             st.NetworkID,
		"device_id":              st.DeviceID,
		"device_name":            st.DeviceName,
		"overlay_ip":             st.OverlayIP,
		"listen_port":            st.ListenPort,
		"nat_type":               st.NATType,
		"observed_addr":          st.ObservedAddr,
		"observed_addr_v6":       st.ObservedAddrV6,
		"first_observed_v6_unix": st.FirstObservedV6Unix,
		"stun_attempts_v4":       st.STUNAttemptsV4,
		"stun_attempts_v6":       st.STUNAttemptsV6,
		"stun_responses_v4":      st.STUNResponsesV4,
		"stun_responses_v6":      st.STUNResponsesV6,
		"disco_enabled":          st.DiscoEnabled,
		"peer_count":             st.PeerCount,
		"phase":                  st.Phase,
		"desired_phase":          st.DesiredPhase,
		"peers":                  st.Peers,
	}
}

// publishScenario records a test-scenario state-change: it caches the
// state (for the stats ticker to re-emit — see republishLastScenario)
// and writes it to Cloud Logging via logScenario. Invoked once per
// apply/revert transition from cloudLoggerReporter.ReportScenario.
func (c *cloudLogger) publishScenario(state, scenarioID, peerDeviceID string, nonce int64, errMsg string) {
	if c == nil {
		return
	}
	rec := scenarioRecord{
		state:        state,
		scenarioID:   scenarioID,
		peerDeviceID: peerDeviceID,
		nonce:        nonce,
		errMsg:       errMsg,
	}
	// Cache the latest transition so the stats ticker can keep it fresh
	// in Cloud Logging even if this one-shot emit's ingestion lags (#592).
	c.lastScenario.Store(&rec)
	c.logScenario(rec)
}

// republishLastScenario re-emits the most recently reported scenario
// state (if any). Called on every stats tick from runStatsPublisher so
// the scenario signal is continuously refreshed and survives per-record
// Cloud Logging ingest jitter under load (#592). No-op before the first
// ReportScenario — which is always the case in production (!testharness)
// builds, where the NoopDispatcher never reports.
func (c *cloudLogger) republishLastScenario() {
	if c == nil {
		return
	}
	if rec := c.lastScenario.Load(); rec != nil {
		c.logScenario(*rec)
	}
}

// logScenario renders a scenarioRecord into the Cloud Logging Entry whose
// label set + payload keys are the testnet-fallback-runner.sh contract
// (it polls for matching scenario_id+nonce+state). Severity Info; payload
// always includes state, scenario_id, peer_device_id, nonce; error is
// included only when non-empty. Shared by the one-shot publishScenario
// and the periodic republishLastScenario so both emit an identical shape.
func (c *cloudLogger) logScenario(rec scenarioRecord) {
	if c == nil || c.logger == nil {
		return
	}
	c.logger.Log(logging.Entry{
		Severity: logging.Info,
		Payload:  scenarioPayload(rec),
		Labels: map[string]string{
			"event":         "waired_test_scenario",
			"instance_name": c.instanceName,
		},
	})
}

// scenarioPayload renders a scenarioRecord into the JSON payload shape
// the testnet fallback runner polls (jsonPayload.state / scenario_id /
// nonce). error is included only when non-empty. Pure so the schema can
// be pinned by a unit test the way buildPayload is.
func scenarioPayload(rec scenarioRecord) map[string]any {
	payload := map[string]any{
		"msg":            "waired_test_scenario",
		"state":          rec.state,
		"scenario_id":    rec.scenarioID,
		"peer_device_id": rec.peerDeviceID,
		"nonce":          rec.nonce,
	}
	if rec.errMsg != "" {
		payload["error"] = rec.errMsg
	}
	return payload
}

// cloudLoggerReporter implements testharness.Reporter on top of the
// lazily-bound cloudLogger pointer. Always emits an slog Info record
// (developer-machine path); when cloudLogger is bound it additionally
// publishes to Cloud Logging.
type cloudLoggerReporter struct {
	cl *atomic.Pointer[cloudLogger]
}

func (r cloudLoggerReporter) ReportScenario(state, scenarioID, peerDeviceID string, nonce int64, errMsg string) {
	slog.Info("waired_test_scenario",
		"state", state,
		"scenario_id", scenarioID,
		"peer_device_id", peerDeviceID,
		"nonce", nonce,
		"error", errMsg,
	)
	if r.cl == nil {
		return
	}
	if cl := r.cl.Load(); cl != nil {
		cl.publishScenario(state, scenarioID, peerDeviceID, nonce, errMsg)
	}
}

// close flushes the Cloud Logging client. Invoked on ctx cancellation
// from runStatsPublisher; the deferred call ensures any final stats
// emit makes it to the API before the agent exits.
func (c *cloudLogger) close(ctx context.Context) {
	if c == nil {
		return
	}
	c.closeMu.Lock()
	defer c.closeMu.Unlock()
	if c.closed {
		return
	}
	c.closed = true
	flushCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	_ = flushCtx
	if err := c.client.Close(); err != nil {
		slog.Warn("cloud-logging: close failed", "err", err)
	}
}

//go:build e2e

// Agent-harness grade e2e (#322): stands up a real engine and the real
// gateway, drives the coding-agent-shaped fixture at the Anthropic
// surface, and reports whether the model under test can actually emit
// structured tool calls under that weight.
//
// It deliberately reuses the production stack rather than talking to
// the engine directly — the whole question is what a coding agent gets
// back through waired, and the Anthropic↔OpenAI translation is part of
// that path. (It is not the SUSPECT: #322 established the translation
// is correct and native. It is in the path because grading the engine
// in isolation would grade something no user ever talks to.)
//
// Run against one model:
//
//	WAIRED_AGENTGRADE_MODEL=qwen3.6:27b-q4_K_M \
//	  go test -tags e2e -run TestAgentGrade -timeout 30m ./internal/e2e/agentgrade/...
//
// Or via the Makefile: make e2e-agentgrade MODEL=<ollama tag>
//
// Skips when ollama is not installed. The model must already be pulled
// or be pullable: a probe run pays a model download once.
package agentgrade_e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/waired-ai/waired-agent/internal/agentgrade"
	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/download"
	"github.com/waired-ai/waired-agent/internal/gateway"
	"github.com/waired-ai/waired-agent/internal/hardware"
	"github.com/waired-ai/waired-agent/internal/router"
	infruntime "github.com/waired-ai/waired-agent/internal/runtime"
)

// defaultModelTag is what runs when no model is named: the catalog's
// withheld CI fixture, so a bare `make e2e-agentgrade` exercises the
// plumbing on the same model the routing sentinel pulls on every PR
// rather than on a tag nothing else uses.
//
// It emits real structured tool calls — that is why it was chosen over
// the smaller qwen2.5-coder-0.5b, which cannot — but a 352M model
// produces nothing worth offering, so it is internal_only in the
// catalog. Expect a grade of "fail": one under-claim on read-file in
// three trials. That is the plumbing working, not a regression.
const defaultModelTag = "granite4:350m"

func modelTag() string {
	if v := strings.TrimSpace(os.Getenv("WAIRED_AGENTGRADE_MODEL")); v != "" {
		return v
	}
	return defaultModelTag
}

// trials raises the sample for one run without moving DefaultTrials,
// which is the count every stored verdict was measured at and must stay
// put for them to remain comparable.
//
// It exists because three trials answers "is this model stable" but not
// "how often does an unstable one fail", and the models sitting at 1/3
// are exactly the ones that second question is about. Re-measuring one
// of them at a higher count is a deliberate, occasional act — hence an
// env knob rather than a new default.
//
// Zero or unparseable means DefaultTrials: a typo'd count silently
// halving the sample would be worse than ignoring it.
func trials(t *testing.T) int {
	v := strings.TrimSpace(os.Getenv("WAIRED_AGENTGRADE_TRIALS"))
	if v == "" {
		return 0
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		t.Fatalf("WAIRED_AGENTGRADE_TRIALS=%q is not a positive integer", v)
	}
	return n
}

// stream selects the SSE path. Off by default because that is what every
// stored verdict was measured on; on is what a coding agent actually
// does, so a re-measurement drives both and compares (#426).
//
// Any value other than the empty string, "0" or "false" turns it on:
// the failure this guards against is a run that silently measured the
// wrong transport, and a strict parser that rejected "yes" would just
// move that failure to the shell.
func stream() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("WAIRED_AGENTGRADE_STREAM"))) {
	case "", "0", "false":
		return false
	default:
		return true
	}
}

func TestAgentGrade(t *testing.T) {
	bin, err := exec.LookPath("ollama")
	if err != nil {
		t.Skipf("ollama not installed: %v", err)
	}
	tag := modelTag()

	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Minute)
	defer cancel()

	base := startStack(t, ctx, bin, tag)

	probe := agentgrade.Probe{BaseURL: base, Trials: trials(t), Stream: stream()}
	rep, err := probe.Run(ctx, "waired/test")
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	rep.Model = tag

	reportTable(t, rep)

	// The assertion is about the PROBE, not the model: a run must
	// produce a verdict for every case. Whether this particular model
	// passes is the measurement, and pinning a third-party model's
	// behaviour as a test expectation would turn an upstream weight
	// change into a red build for no defect of ours.
	if len(rep.Results) != len(agentgrade.Cases) {
		t.Fatalf("got %d results for %d cases", len(rep.Results), len(agentgrade.Cases))
	}
	if rep.Grade == agentgrade.GradeUnknown {
		t.Fatalf("model could not be graded: %s", rep.Error)
	}

	if out := strings.TrimSpace(os.Getenv("WAIRED_AGENTGRADE_JSON")); out != "" {
		writeJSON(t, out, rep)
	}
}

// reportTable prints the per-case verdicts. This IS the deliverable —
// the grade alone does not tell a maintainer whether the model cannot
// format calls at all or merely over-calls on small talk.
func reportTable(t *testing.T, rep agentgrade.Report) {
	t.Helper()
	t.Logf("=== agent-harness grade: %s → %s (%d trials, %s transport, %s)",
		rep.Model, rep.Grade, rep.Trials, rep.Transport, rep.Duration)
	if len(rep.Flaky) > 0 {
		// Say it loudly. A model whose verdict changes between runs is a
		// different problem from one that is simply bad, and the whole
		// reason trials exist is that this was invisible before.
		t.Logf("  NOT REPRODUCIBLE across trials: %s", strings.Join(rep.Flaky, ", "))
	}
	for _, r := range rep.Results {
		t.Logf("  %-18s %-32s %s", r.Case, r.Verdict, r.Detail)
		if r.Evidence != "" {
			t.Logf("      evidence: %s", oneLine(r.Evidence))
		}
		if r.Text != "" {
			t.Logf("      text:     %s", oneLine(r.Text))
		}
	}
}

func oneLine(s string) string {
	s = strings.ReplaceAll(s, "\n", " ⏎ ")
	if len(s) > 300 {
		s = s[:300] + "…"
	}
	return s
}

func writeJSON(t *testing.T, path string, rep agentgrade.Report) {
	t.Helper()
	b, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		t.Errorf("marshal report: %v", err)
		return
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Errorf("write %s: %v", path, err)
		return
	}
	t.Logf("report written to %s", path)
}

// startStack brings up ollama + the production gateway and returns the
// Anthropic base URL to drive.
func startStack(t *testing.T, ctx context.Context, bin, tag string) string {
	t.Helper()

	port := freeTCPPort(t)
	adapter := infruntime.NewOllamaAdapter(infruntime.OllamaConfig{
		Binary:         bin,
		Host:           "127.0.0.1",
		Port:           port,
		Spawner:        infruntime.DefaultSpawner{},
		HealthInterval: 500 * time.Millisecond,
		HealthSuccess:  1,
		HealthMaxFails: 120,
		StopTimeout:    10 * time.Second,
	})
	if err := adapter.EnsureRunning(ctx); err != nil {
		t.Fatalf("EnsureRunning: %v", err)
	}
	t.Cleanup(func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = adapter.Stop(stopCtx)
	})
	t.Logf("ollama listening on %s", adapter.BaseURL())

	t.Setenv("OLLAMA_HOST", fmt.Sprintf("127.0.0.1:%d", port))
	puller := download.NewPuller(bin, download.DefaultRunner{})
	t.Logf("ensuring %s is pulled", tag)
	if err := puller.Pull(ctx, tag, func(p download.Progress) {
		if p.State == download.StatePulling && p.Percent%25 == 0 && p.Percent > 0 {
			t.Logf("  pull: %s %d%%", p.State, p.Percent)
		}
	}); err != nil {
		t.Fatalf("pull %s: %v", tag, err)
	}

	// The fixture's session context alone is ~30 KB, so a 4k window
	// would truncate the request before the model ever saw the tools.
	// 64k is comfortably above the ~25k-token fixture without demanding
	// a window every host can serve.
	manifest := catalog.Manifest{
		ModelID:       "agentgrade-subject",
		ModelAliases:  []string{"waired/test"},
		ContextLength: 65536,
		Capabilities:  []string{"chat", "tool_use", "json_mode"},
		Variants: []catalog.Variant{{
			VariantID: "probe", Format: catalog.FormatOllamaTag,
			RuntimeSupport: []string{catalog.RuntimeOllama},
			Source:         catalog.VariantSource{Type: "ollama", Tag: tag},
		}},
	}
	state := catalog.State{
		Version: catalog.StateVersion,
		Models: map[string]catalog.ModelState{
			"agentgrade-subject": {VariantID: "probe", OllamaTag: tag, State: catalog.ModelStateReady},
		},
		Endpoints: map[string]catalog.EndpointState{},
	}

	registry := infruntime.NewRegistry()
	registry.Register(adapter)

	gwPort := freeTCPPort(t)
	sel := &fixedSelector{manifests: []catalog.Manifest{manifest}, state: state, registry: registry}
	gw := gateway.NewServer(gateway.ServerConfig{}, gateway.Deps{
		Selector:       sel,
		Runtimes:       registry,
		ListManifests:  func() []catalog.Manifest { return []catalog.Manifest{manifest} },
		HTTPClient:     &http.Client{Timeout: 10 * time.Minute},
		AllowOpenAI:    true,
		AllowAnthropic: true,
	})
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", gwPort))
	if err != nil {
		t.Fatalf("listen gateway: %v", err)
	}
	gwCtx, gwCancel := context.WithCancel(ctx)
	t.Cleanup(gwCancel)
	go func() { _ = gw.Serve(gwCtx, ln) }()

	gwBase := fmt.Sprintf("http://127.0.0.1:%d", gwPort)
	waitForGateway(t, gwBase)
	return gwBase + "/anthropic"
}

type fixedSelector struct {
	manifests []catalog.Manifest
	state     catalog.State
	registry  *infruntime.Registry
}

func (f *fixedSelector) inputs() router.Inputs {
	return router.Inputs{
		Manifests:  f.manifests,
		LocalState: f.state,
		Hardware:   hardware.Profile{OS: "linux", Arch: "x86_64", RAMTotalGB: 64},
		Runtimes:   f.registry,
	}
}

func (f *fixedSelector) Select(ctx context.Context, req router.Request) (router.Selection, error) {
	return router.NewSelector(f.inputs()).Select(ctx, req)
}

func (f *fixedSelector) SelectK(ctx context.Context, req router.Request, k int) ([]router.Candidate, error) {
	return router.NewSelector(f.inputs()).SelectK(ctx, req, k)
}

func freeTCPPort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func waitForGateway(t *testing.T, base string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(base + "/v1/models")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("gateway never came up at %s", base)
}

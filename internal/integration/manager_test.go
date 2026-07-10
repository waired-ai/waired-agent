package integration

import (
	"context"
	"errors"
	"testing"
)

// fakeAdapter is a scriptable Adapter for the manager tests.
type fakeAdapter struct {
	id        AgentID
	detect    Detection
	detectErr error
	applyErr  error
	auditOut  []AuditFinding
	auditErr  error
	uninstall error

	// Counters so tests can assert on call patterns.
	detectCalls    int
	applyCalls     int
	auditCalls     int
	uninstallCalls int
}

func (f *fakeAdapter) ID() AgentID { return f.id }
func (f *fakeAdapter) Detect(_ context.Context, _ ApplyOptions) (Detection, error) {
	f.detectCalls++
	return f.detect, f.detectErr
}
func (f *fakeAdapter) Apply(_ context.Context, _ ApplyOptions) error {
	f.applyCalls++
	return f.applyErr
}
func (f *fakeAdapter) Audit(_ context.Context, _ ApplyOptions) ([]AuditFinding, error) {
	f.auditCalls++
	return f.auditOut, f.auditErr
}
func (f *fakeAdapter) Uninstall(_ context.Context, _ ApplyOptions) error {
	f.uninstallCalls++
	return f.uninstall
}

func TestManager_AgentIDs_Sorted(t *testing.T) {
	m := NewManager(
		&fakeAdapter{id: AgentOpenCode},
		&fakeAdapter{id: AgentClaudeCode},
	)
	got := m.AgentIDs()
	if len(got) != 2 {
		t.Fatalf("len = %d", len(got))
	}
	if got[0] != AgentClaudeCode || got[1] != AgentOpenCode {
		// alphabetical: claude-code < opencode
		t.Fatalf("sort order: %v", got)
	}
}

func TestManager_ApplyAll_DetectGate(t *testing.T) {
	cc := &fakeAdapter{id: AgentClaudeCode, detect: Detection{Found: true}}
	oc := &fakeAdapter{id: AgentOpenCode, detect: Detection{Found: false}}
	m := NewManager(cc, oc)

	results := m.ApplyAll(context.Background(), ApplyOptions{})
	if len(results) != 2 {
		t.Fatalf("results = %d", len(results))
	}
	for _, r := range results {
		switch r.Agent {
		case AgentClaudeCode:
			if !r.Applied || r.Skipped || r.Err != nil {
				t.Fatalf("claude-code result %+v", r)
			}
		case AgentOpenCode:
			if r.Applied || !r.Skipped || r.Err != nil {
				t.Fatalf("opencode result %+v", r)
			}
		}
	}
	if cc.applyCalls != 1 || oc.applyCalls != 0 {
		t.Fatalf("apply calls cc=%d oc=%d", cc.applyCalls, oc.applyCalls)
	}
}

func TestManager_ApplyAll_ForceBypassesDetect(t *testing.T) {
	oc := &fakeAdapter{id: AgentOpenCode, detect: Detection{Found: false}}
	m := NewManager(oc)

	results := m.ApplyAll(context.Background(), ApplyOptions{Force: true})
	if len(results) != 1 || !results[0].Applied {
		t.Fatalf("force results: %+v", results)
	}
	// Detect must not be called when Force is true (it's wasted work
	// and adapters may rely on this skip when unit-testing apply).
	if oc.detectCalls != 0 {
		t.Fatalf("detect calls under force = %d, want 0", oc.detectCalls)
	}
	if oc.applyCalls != 1 {
		t.Fatalf("apply calls under force = %d", oc.applyCalls)
	}
}

func TestManager_ApplyAll_PropagatesErrors(t *testing.T) {
	boom := errors.New("boom")
	cc := &fakeAdapter{id: AgentClaudeCode, detect: Detection{Found: true}, applyErr: boom}
	oc := &fakeAdapter{id: AgentOpenCode, detect: Detection{Found: true}}
	m := NewManager(cc, oc)

	results := m.ApplyAll(context.Background(), ApplyOptions{})
	if len(results) != 2 {
		t.Fatalf("results = %d", len(results))
	}
	// Failure on cc must not short-circuit oc.
	for _, r := range results {
		if r.Agent == AgentClaudeCode && !errors.Is(r.Err, boom) {
			t.Fatalf("claude-code Err = %v, want %v", r.Err, boom)
		}
		if r.Agent == AgentOpenCode && !r.Applied {
			t.Fatalf("opencode should still apply: %+v", r)
		}
	}
}

func TestManager_AuditAll_WrapsAdapterErrors(t *testing.T) {
	boom := errors.New("audit boom")
	cc := &fakeAdapter{id: AgentClaudeCode, auditErr: boom}
	oc := &fakeAdapter{id: AgentOpenCode, auditOut: []AuditFinding{{
		Status: StatusOK, Subject: "opencode skills",
	}}}
	m := NewManager(cc, oc)

	all, err := m.AuditAll(context.Background(), ApplyOptions{})
	if err != nil {
		t.Fatalf("AuditAll err = %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("findings = %d", len(all))
	}
	var sawCC, sawOC bool
	for _, f := range all {
		if f.Status == StatusFail && f.Subject == "claude-code audit" {
			sawCC = true
		}
		if f.Status == StatusOK && f.Subject == "opencode skills" {
			sawOC = true
		}
	}
	if !sawCC || !sawOC {
		t.Fatalf("findings: %+v", all)
	}
}

func TestManager_UninstallAll_CallsEverybody(t *testing.T) {
	cc := &fakeAdapter{id: AgentClaudeCode}
	oc := &fakeAdapter{id: AgentOpenCode}
	m := NewManager(cc, oc)
	results := m.UninstallAll(context.Background(), ApplyOptions{})
	if cc.uninstallCalls != 1 || oc.uninstallCalls != 1 {
		t.Fatalf("uninstall counts cc=%d oc=%d", cc.uninstallCalls, oc.uninstallCalls)
	}
	if len(results) != 2 {
		t.Fatalf("results = %d", len(results))
	}
}

func TestManager_AdapterMissing(t *testing.T) {
	m := NewManager()
	_, err := m.Adapter(AgentClaudeCode)
	if !errors.Is(err, ErrAgentNotFound) {
		t.Fatalf("err = %v", err)
	}
	res := m.ApplyOne(context.Background(), AgentClaudeCode, ApplyOptions{})
	if !errors.Is(res.Err, ErrAgentNotFound) {
		t.Fatalf("ApplyOne missing agent err = %v", res.Err)
	}
}

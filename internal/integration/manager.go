package integration

import (
	"context"
	"fmt"
	"sort"
)

// Manager is the registry of Adapters and the entry point for
// `waired link [agent]`, `waired init`'s integration phase, and
// `waired doctor`. It is safe to construct a fresh Manager per call;
// adapters are stateless.
type Manager struct {
	adapters map[AgentID]Adapter
}

// NewManager constructs a Manager pre-loaded with the given adapters.
// Pass them in any order; ApplyAll iterates in stable AgentID order.
func NewManager(adapters ...Adapter) *Manager {
	m := &Manager{adapters: map[AgentID]Adapter{}}
	for _, a := range adapters {
		m.adapters[a.ID()] = a
	}
	return m
}

// Register adds (or replaces) an adapter.
func (m *Manager) Register(a Adapter) { m.adapters[a.ID()] = a }

// Adapter returns the registered adapter for id.
func (m *Manager) Adapter(id AgentID) (Adapter, error) {
	a, ok := m.adapters[id]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrAgentNotFound, id)
	}
	return a, nil
}

// AgentIDs returns the sorted list of registered AgentIDs.
func (m *Manager) AgentIDs() []AgentID {
	ids := make([]AgentID, 0, len(m.adapters))
	for id := range m.adapters {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}

// ApplyResult summarises one Apply pass per agent.
type ApplyResult struct {
	Agent   AgentID
	Applied bool  // false when the agent was Detect()-skipped
	Skipped bool  // true when Detect was negative and Force=false
	Err     error // nil on success or non-fatal skip
	Notes   []string
}

// ApplyAll runs Apply against every registered adapter that Detect()s
// as installed, except when opts.Force=true (then everyone runs).
//
// Errors are collected per-adapter; the manager does NOT short-circuit
// on the first failure — callers (waired init) decide whether to abort
// based on the aggregate. waired init defaults to fail-fast: any
// non-skipped non-nil ApplyResult.Err triggers a non-zero exit.
func (m *Manager) ApplyAll(ctx context.Context, opts ApplyOptions) []ApplyResult {
	results := make([]ApplyResult, 0, len(m.adapters))
	for _, id := range m.AgentIDs() {
		a := m.adapters[id]
		results = append(results, m.applyOne(ctx, a, opts))
	}
	return results
}

// ApplyOne runs Apply for a single agent. Force is honoured; when
// Force=false and Detect is negative, the result is Skipped=true with
// Err=nil so the CLI can print "skipped: not installed" without
// failing the whole pass.
func (m *Manager) ApplyOne(ctx context.Context, id AgentID, opts ApplyOptions) ApplyResult {
	a, err := m.Adapter(id)
	if err != nil {
		return ApplyResult{Agent: id, Err: err}
	}
	return m.applyOne(ctx, a, opts)
}

func (m *Manager) applyOne(ctx context.Context, a Adapter, opts ApplyOptions) ApplyResult {
	res := ApplyResult{Agent: a.ID()}
	if !opts.Force {
		det, err := a.Detect(ctx, opts)
		if err != nil {
			res.Err = fmt.Errorf("detect: %w", err)
			return res
		}
		res.Notes = det.Notes
		if !det.Found {
			res.Skipped = true
			return res
		}
	}
	if err := a.Apply(ctx, opts); err != nil {
		res.Err = err
		return res
	}
	res.Applied = true
	return res
}

// AuditAll runs Audit against every registered adapter and concatenates
// findings. Detect failures surface as a single StatusFail finding so
// `waired doctor` can render them the same way as other problems.
func (m *Manager) AuditAll(ctx context.Context, opts ApplyOptions) ([]AuditFinding, error) {
	var all []AuditFinding
	for _, id := range m.AgentIDs() {
		a := m.adapters[id]
		findings, err := a.Audit(ctx, opts)
		if err != nil {
			all = append(all, AuditFinding{
				Status:  StatusFail,
				Subject: fmt.Sprintf("%s audit", a.ID()),
				Detail:  err.Error(),
			})
			continue
		}
		all = append(all, findings...)
	}
	return all, nil
}

// UninstallAll runs Uninstall for every registered adapter. Errors are
// collected; callers decide whether to abort.
func (m *Manager) UninstallAll(ctx context.Context, opts ApplyOptions) []ApplyResult {
	results := make([]ApplyResult, 0, len(m.adapters))
	for _, id := range m.AgentIDs() {
		a := m.adapters[id]
		res := ApplyResult{Agent: a.ID()}
		if err := a.Uninstall(ctx, opts); err != nil {
			res.Err = err
		} else {
			res.Applied = true
		}
		results = append(results, res)
	}
	return results
}

// UninstallOne runs Uninstall for a single agent.
func (m *Manager) UninstallOne(ctx context.Context, id AgentID, opts ApplyOptions) ApplyResult {
	a, err := m.Adapter(id)
	if err != nil {
		return ApplyResult{Agent: id, Err: err}
	}
	if err := a.Uninstall(ctx, opts); err != nil {
		return ApplyResult{Agent: id, Err: err}
	}
	return ApplyResult{Agent: id, Applied: true}
}

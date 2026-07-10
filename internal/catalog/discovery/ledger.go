package discovery

import (
	"encoding/json"
	"fmt"
)

// Seen-ledger statuses.
const (
	// StatusCandidate: surfaced in the radar, not yet acted on. Re-surfaces.
	StatusCandidate = "candidate"
	// StatusFlagged: a draft PR was opened. Terminal — never re-proposed.
	StatusFlagged = "flagged"
	// StatusDismissed: a maintainer rejected it. Terminal — never re-proposed.
	StatusDismissed = "dismissed"
)

// SeenEntry records a repo's discovery history so the radar neither re-flags a
// model every week nor re-proposes one a maintainer already dismissed.
type SeenEntry struct {
	FirstSeen string `json:"first_seen"`
	LastSeen  string `json:"last_seen"`
	Status    string `json:"status"`
	Note      string `json:"note,omitempty"`
}

// Ledger is the persisted, git-tracked seen-state (discovery/seen.json). It is
// written back by the radar's shell step, never by the LLM.
type Ledger struct {
	Schema  int                  `json:"schema"`
	Entries map[string]SeenEntry `json:"entries"`
}

// LoadLedger parses a seen.json payload. Empty input yields an empty ledger.
func LoadLedger(data []byte) (Ledger, error) {
	if len(data) == 0 {
		return Ledger{Schema: 1, Entries: map[string]SeenEntry{}}, nil
	}
	var l Ledger
	if err := json.Unmarshal(data, &l); err != nil {
		return Ledger{}, fmt.Errorf("discovery: parse seen ledger: %w", err)
	}
	if l.Entries == nil {
		l.Entries = map[string]SeenEntry{}
	}
	if l.Schema == 0 {
		l.Schema = 1
	}
	return l, nil
}

// ShouldSkip reports whether a repo is in a terminal (flagged/dismissed) state.
func (l Ledger) ShouldSkip(repoID string) bool {
	e, ok := l.Entries[repoID]
	if !ok {
		return false
	}
	return e.Status == StatusFlagged || e.Status == StatusDismissed
}

// Record upserts a repo's status. `now` is an RFC3339 timestamp supplied by the
// caller (the package stays pure). first_seen is preserved across updates.
func (l *Ledger) Record(repoID, status, now string) {
	if l.Entries == nil {
		l.Entries = map[string]SeenEntry{}
	}
	e, ok := l.Entries[repoID]
	if !ok {
		e = SeenEntry{FirstSeen: now}
	}
	e.LastSeen = now
	e.Status = status
	l.Entries[repoID] = e
}

// Marshal renders the ledger as stable, indented JSON.
func (l Ledger) Marshal() ([]byte, error) {
	if l.Schema == 0 {
		l.Schema = 1
	}
	if l.Entries == nil {
		l.Entries = map[string]SeenEntry{}
	}
	b, err := json.MarshalIndent(l, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

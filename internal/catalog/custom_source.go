package catalog

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/waired-ai/waired-agent/internal/platform/paths"
	"github.com/waired-ai/waired-agent/internal/platform/secrets"
)

// Custom models are models a person imported from Hugging Face in the
// console (waired-ai/waired#1473). The control plane holds them; an agent
// fetches its account's set (POST /v1/devices/self/custom-models) when
// the revision on its own network-map entry changes, and keeps the last
// set here so that a start without the control plane still resolves them.
//
// Three views, each listing only custom models — callers put them after
// the bundled catalog, so a bundled id or retired name always resolves
// first:
//
//   - Serveable: the owner's own models, plus any the account deleted
//     while this device still used them (withdrawn). Pull, start and the
//     desired model resolve against these.
//   - Routable: Serveable plus the routing copies of what the owner's
//     teammates imported. The router matches a peer's tag against these;
//     a teammate's model can be routed to, never installed here.
//   - Offerable: the owner's own models. The pickers list these.

// CustomSource holds the custom models this device may use.
type CustomSource struct {
	path    string
	bundled []Manifest
	logger  *slog.Logger

	mu   sync.Mutex // serialises Replace and the file
	cur  atomic.Pointer[customSnapshot]
	subs []func()
}

type customSnapshot struct {
	Version   int        `json:"version"`
	NetworkID string     `json:"network_id"`
	Revision  string     `json:"revision"`
	FetchedAt time.Time  `json:"fetched_at"`
	Own       []Manifest `json:"own,omitempty"`
	Team      []Manifest `json:"team,omitempty"`
	Withdrawn []Manifest `json:"withdrawn,omitempty"`
	// Unreadable is why each of the account's own models in the last
	// fetch was dropped, by id: an entry this build does not accept. Not
	// kept on disk; it answers "why is a model the account holds not
	// here" for the set fetched by this process (waired-ai/waired#1480).
	Unreadable map[string]string `json:"-"`
}

const customSnapshotVersion = 1

// DefaultCustomModelsPath is <StateDir>/inference/custom-models.json.
func DefaultCustomModelsPath() string {
	return filepath.Join(paths.StateDir(paths.AutoDetect), "inference", "custom-models.json")
}

// NewCustomSource loads the kept set from path. A file written for another
// network — the device was enrolled again, possibly into another account
// — is ignored, and so is one that does not decode: the next fetch
// replaces it. Entries that no longer validate are dropped with a log line
// rather than failing the start.
func NewCustomSource(path, networkID string, bundled []Manifest, logger *slog.Logger) *CustomSource {
	if logger == nil {
		logger = slog.Default()
	}
	s := &CustomSource{path: path, bundled: bundled, logger: logger}
	snap := &customSnapshot{Version: customSnapshotVersion, NetworkID: networkID}
	data, err := os.ReadFile(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		logger.Warn("custom models: kept set unreadable; starting empty", "path", path, "err", err)
	default:
		var kept customSnapshot
		if jerr := json.Unmarshal(data, &kept); jerr != nil {
			logger.Warn("custom models: kept set does not decode; starting empty", "path", path, "err", jerr)
		} else if networkID != "" && kept.NetworkID != networkID {
			logger.Info("custom models: kept set belongs to another network; starting empty", "path", path)
		} else {
			kept.Own = s.valid(kept.Own, false)
			kept.Team = s.valid(kept.Team, true)
			kept.Withdrawn = s.valid(kept.Withdrawn, false)
			if networkID != "" {
				kept.NetworkID = networkID
			}
			snap = &kept
		}
	}
	s.cur.Store(snap)
	return s
}

func (s *CustomSource) valid(ms []Manifest, projection bool) []Manifest {
	return s.validNoting(ms, projection, nil)
}

// validNoting is valid that also records, in dropped, why each entry was
// dropped. A nil dropped records nothing.
func (s *CustomSource) validNoting(ms []Manifest, projection bool, dropped map[string]string) []Manifest {
	out := make([]Manifest, 0, len(ms))
	for _, m := range ms {
		var err error
		if projection {
			err = ValidateCustomProjection(m, s.bundled)
		} else {
			err = ValidateCustomManifest(m, s.bundled)
		}
		if err != nil {
			s.logger.Warn("custom models: dropping an entry that does not validate", "model_id", m.ModelID, "err", err)
			if dropped != nil && m.ModelID != "" {
				dropped[m.ModelID] = err.Error()
			}
			continue
		}
		out = append(out, m)
	}
	return out
}

func (s *CustomSource) snap() *customSnapshot {
	if s == nil {
		return &customSnapshot{}
	}
	return s.cur.Load()
}

// Revision is the revision of the set last applied, "" before any fetch.
func (s *CustomSource) Revision() string { return s.snap().Revision }

// Serveable lists the models this device may pull and serve.
func (s *CustomSource) Serveable() []Manifest {
	sn := s.snap()
	return slices.Concat(sn.Own, sn.Withdrawn)
}

// Routable lists every custom model a router may match a peer against.
func (s *CustomSource) Routable() []Manifest {
	sn := s.snap()
	out := slices.Concat(sn.Own, sn.Withdrawn)
	for _, m := range sn.Team {
		if !containsModel(out, m.ModelID) {
			out = append(out, m)
		}
	}
	return out
}

// Offerable lists the models a picker shows: the owner's own.
func (s *CustomSource) Offerable() []Manifest { return slices.Clone(s.snap().Own) }

// Withdrawn lists the models the account deleted that this device still
// used when the change arrived.
func (s *CustomSource) Withdrawn() []Manifest { return slices.Clone(s.snap().Withdrawn) }

// Unreadable reports why the account's own model modelID, present in the
// last fetch, was dropped: this build does not accept the entry. False when
// the last fetch did not hold it at all, or held it and kept it.
func (s *CustomSource) Unreadable(modelID string) (string, bool) {
	why, ok := s.snap().Unreadable[modelID]
	return why, ok
}

func containsModel(ms []Manifest, id string) bool {
	return slices.ContainsFunc(ms, func(m Manifest) bool { return m.ModelID == id })
}

// Subscribe registers fn to run after every Replace that changed the set.
func (s *CustomSource) Subscribe(fn func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.subs = append(s.subs, fn)
}

// Replace applies a fetched set. Every entry is validated; one that does
// not is dropped and logged. A model the set no longer holds but inUse
// reports this device as serving, choosing or keeping is moved to
// Withdrawn instead of dropped, so the device keeps serving it and says
// so (waired-ai/waired#1473: nothing falls back to a bundled model
// silently). It returns the ids newly withdrawn.
func (s *CustomSource) Replace(set CustomModelSet, inUse func(modelID string) bool) ([]string, error) {
	s.mu.Lock()
	prev := s.cur.Load()
	next := &customSnapshot{
		Version:   customSnapshotVersion,
		NetworkID: prev.NetworkID,
		Revision:  set.Revision,
		FetchedAt: time.Now().UTC(),
	}
	unreadable := map[string]string{}
	next.Own = s.validNoting(set.Own, false, unreadable)
	if len(unreadable) > 0 {
		next.Unreadable = unreadable
	}
	for _, m := range s.valid(set.Team, true) {
		if !containsModel(next.Own, m.ModelID) {
			next.Team = append(next.Team, m)
		}
	}
	var newlyWithdrawn []string
	for _, m := range slices.Concat(prev.Own, prev.Withdrawn) {
		if containsModel(next.Own, m.ModelID) || containsModel(next.Withdrawn, m.ModelID) {
			continue
		}
		if inUse != nil && inUse(m.ModelID) {
			next.Withdrawn = append(next.Withdrawn, m)
			if !containsModel(prev.Withdrawn, m.ModelID) {
				newlyWithdrawn = append(newlyWithdrawn, m.ModelID)
			}
		}
	}
	err := s.saveLocked(next)
	changed := !sameSet(prev, next)
	s.cur.Store(next)
	subs := slices.Clone(s.subs)
	s.mu.Unlock()
	if changed {
		for _, fn := range subs {
			fn()
		}
	}
	return newlyWithdrawn, err
}

func sameSet(a, b *customSnapshot) bool {
	ids := func(ms []Manifest) []string {
		out := make([]string, 0, len(ms))
		for _, m := range ms {
			raw, _ := json.Marshal(m)
			out = append(out, string(raw))
		}
		slices.Sort(out)
		return out
	}
	return slices.Equal(ids(a.Own), ids(b.Own)) && slices.Equal(ids(a.Team), ids(b.Team)) &&
		slices.Equal(ids(a.Withdrawn), ids(b.Withdrawn))
}

func (s *CustomSource) saveLocked(snap *customSnapshot) error {
	if s.path == "" {
		return nil
	}
	if err := secrets.SecureDir(filepath.Dir(s.path)); err != nil {
		return fmt.Errorf("custom models: mkdir: %w", err)
	}
	data, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return fmt.Errorf("custom models: marshal: %w", err)
	}
	if err := secrets.WriteSecret(s.path, data); err != nil {
		return fmt.Errorf("custom models: write: %w", err)
	}
	return nil
}

// SetNetwork empties the set when the device now belongs to a different
// network than the one the kept set was fetched for — it was enrolled
// again, possibly into another account. The next fetch fills it.
func (s *CustomSource) SetNetwork(networkID string) {
	if s == nil || networkID == "" {
		return
	}
	s.mu.Lock()
	prev := s.cur.Load()
	if prev.NetworkID == networkID {
		s.mu.Unlock()
		return
	}
	next := &customSnapshot{Version: customSnapshotVersion, NetworkID: networkID}
	if prev.NetworkID == "" {
		// Nothing was kept yet: adopt the network.
		cp := *prev
		cp.NetworkID = networkID
		next = &cp
	}
	if err := s.saveLocked(next); err != nil {
		s.logger.Warn("custom models: could not keep the reset set", "err", err)
	}
	s.cur.Store(next)
	subs := slices.Clone(s.subs)
	s.mu.Unlock()
	for _, fn := range subs {
		fn()
	}
}

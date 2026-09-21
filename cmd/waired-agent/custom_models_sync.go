package main

import (
	"context"
	"crypto/ed25519"
	"log/slog"
	"path/filepath"
	"slices"
	"sync/atomic"
	"time"

	"github.com/waired-ai/waired-agent/internal/agentconfig"
	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/identity"
	"github.com/waired-ai/waired-agent/internal/notice"
)

// Custom models (waired-ai/waired#1473): the control plane puts a revision
// on this device's own network-map entry, and this worker fetches the set
// when the revision is one it does not hold. Nothing is fetched until a
// revision has been seen: a control plane that does not write one does not
// have the endpoint either. After that it also fetches every
// customModelsRefresh, and whenever something asks for a custom model it
// does not have yet (Kick).

// customModelsRefresh is the backstop fetch interval, for a revision bump
// that was lost.
const customModelsRefresh = 30 * time.Minute

// customModelsRetry is the wait after a failed fetch.
const customModelsRetry = 30 * time.Second

type customModelsFetcher interface {
	FetchCustomModels(ctx context.Context, deviceID, haveRevision string, machineKey ed25519.PrivateKey) (catalog.CustomModelSet, error)
}

type customModelsSync struct {
	src        *catalog.CustomSource
	client     customModelsFetcher
	deviceID   string
	machineKey ed25519.PrivateKey
	// inUse reports whether this device serves or chose a model, so a
	// model the account deleted is withdrawn rather than dropped.
	inUse  func(modelID string) bool
	logger *slog.Logger

	kick chan struct{}
	seen atomic.Bool // a revision has been seen on the map
}

func newCustomModelsSync(src *catalog.CustomSource, client customModelsFetcher, deviceID string, mk ed25519.PrivateKey,
	inUse func(string) bool, logger *slog.Logger) *customModelsSync {
	return &customModelsSync{
		src: src, client: client, deviceID: deviceID, machineKey: mk,
		inUse: inUse, logger: logger,
		kick: make(chan struct{}, 1),
	}
}

// NoteRevision is called with the revision on every map frame's own
// entry. A revision this device does not hold starts a fetch.
func (s *customModelsSync) NoteRevision(rev string) {
	if s == nil || rev == "" {
		return
	}
	s.seen.Store(true)
	if rev != s.src.Revision() {
		s.Kick()
	}
}

// Kick asks for a fetch without waiting for it. It does nothing until a
// revision has been seen.
func (s *customModelsSync) Kick() {
	if s == nil || !s.seen.Load() {
		return
	}
	select {
	case s.kick <- struct{}{}:
	default:
	}
}

// Run fetches on every kick, refresh and retry, until ctx ends.
func (s *customModelsSync) Run(ctx context.Context) {
	refresh := time.NewTicker(customModelsRefresh)
	defer refresh.Stop()
	retry := time.NewTimer(customModelsRetry)
	retry.Stop()
	defer retry.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.kick:
		case <-refresh.C:
			if !s.seen.Load() {
				continue
			}
		case <-retry.C:
		}
		if err := s.fetch(ctx); err != nil {
			if ctx.Err() != nil {
				return
			}
			s.logger.Warn("custom models: fetch failed; keeping the set held", "err", err, "retry_in", customModelsRetry)
			retry.Reset(customModelsRetry)
		}
	}
}

func (s *customModelsSync) fetch(ctx context.Context) error {
	fctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	set, err := s.client.FetchCustomModels(fctx, s.deviceID, s.src.Revision(), s.machineKey)
	if err != nil {
		return err
	}
	withdrawn, err := s.src.Replace(set, s.inUse)
	if err != nil {
		// The set is applied in memory; only keeping it failed, so a start
		// without the control plane would not see it.
		s.logger.Warn("custom models: could not keep the set", "err", err)
	}
	s.logger.Info("custom models: set applied", "revision", set.Revision, "own", len(set.Own), "team", len(set.Team))
	if len(withdrawn) > 0 {
		s.logger.Warn("custom models: deleted from the account while this computer uses them; kept until it stops", "models", withdrawn)
	}
	return nil
}

// newCustomModelSource loads the kept set from <stateDir>/inference. The
// network is the enrolled identity's when there is one; activate names it
// again, which empties a set kept for another network.
func newCustomModelSource(stateDir string, logger *slog.Logger) *catalog.CustomSource {
	bundled, err := catalog.BundledManifestsIncludingInternal()
	if err != nil {
		logger.Warn("custom models: bundled catalog unreadable; custom entries are checked without it", "err", err)
	}
	networkID := ""
	if id, err := identity.Load(stateDir); err == nil && id != nil {
		networkID = id.NetworkID
	}
	return catalog.NewCustomSource(filepath.Join(stateDir, "inference", "custom-models.json"), networkID, bundled, logger)
}

// customModelInUse reports whether this device serves or has been asked to
// serve modelID: the active model, or the desired one the control plane or
// a person here chose.
func customModelInUse(sub *inferenceSubsystem, modelID string) bool {
	if sub == nil || sub.provider == nil {
		return false
	}
	if active, ok := sub.provider.ActiveModelID(); ok && active == modelID {
		return true
	}
	return sub.provider.preferredModelID() == modelID
}

// preferredModelID is the model the preference file names, whoever chose
// it: a person here, the control plane's desired model, or setup.
func (p *agentInferenceProvider) preferredModelID() string {
	if p == nil || p.preferencePath == "" {
		return ""
	}
	pref, ok, err := agentconfig.LoadPreference(p.preferencePath)
	if err != nil || !ok {
		return ""
	}
	return pref.ModelID
}

// knowsModel reports whether modelID resolves on this device: bundled, or
// a custom model it holds.
func (p *agentInferenceProvider) knowsModel(modelID string) bool {
	return slices.ContainsFunc(p.catalogManifests(), func(m catalog.Manifest) bool { return m.ModelID == modelID })
}

// customModelServedOrChosen reports whether this device serves a custom
// model or has been told to move to one. Public Share guests are refused
// for as long as it holds (waired-ai/waired#1473 ruling 4).
func customModelServedOrChosen(sub *inferenceSubsystem) bool {
	if sub == nil || sub.provider == nil {
		return false
	}
	if active, ok := sub.provider.ActiveModelID(); ok && catalog.IsCustomModelID(active) {
		return true
	}
	return catalog.IsCustomModelID(sub.provider.preferredModelID())
}

// publishCustomModelNotices republishes one notice per custom model the
// account deleted while this computer still uses it (waired-ai/waired#1473).
// Derived on every call: once another model is chosen the model is no
// longer in use, the notice stops being published and lapses, and the next
// fetch drops the model.
func (p *agentInferenceProvider) publishCustomModelNotices(context.Context) {
	if p == nil || p.notices == nil {
		return
	}
	p.notices.Publish(noticeSourceCustomModels, p.customModelNotices())
}

func (p *agentInferenceProvider) customModelNotices() []notice.Notice {
	var out []notice.Notice
	active, _ := p.ActiveModelID()
	preferred := p.preferredModelID()
	for _, m := range p.custom.Withdrawn() {
		if m.ModelID != active && m.ModelID != preferred {
			continue
		}
		name := m.DisplayName
		if name == "" {
			name = m.ModelID
		}
		out = append(out, notice.CustomModelWithdrawn(name))
	}
	return out
}

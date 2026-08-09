package management

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/waired-ai/waired-agent/internal/agentconfig"
	"github.com/waired-ai/waired-agent/internal/catalog"
)

// PreferredModelRequest is the body of POST /waired/v1/inference/preferred-model.
// Exactly one of the two fields is set: a model choice names ModelID, and
// the install flow's "don't download a model now" (waired-agent#586) sends
// {"none":true}. A body with both — a name AND the statement that there is
// no name — is contradictory and refused.
type PreferredModelRequest struct {
	ModelID string `json:"model_id"`
	None    bool   `json:"none,omitempty"`
}

// PreferredModelResponse is the 202-Accepted body. WillRestart is false
// when the switch applies in process (#812, the common case) and true when
// the agent falls back to the supervised restart (cross-engine target,
// wedged engine, or an older/unwired daemon). Clients must honour the field
// rather than assume a restart.
type PreferredModelResponse struct {
	ModelID     string `json:"model_id"`
	WillRestart bool   `json:"will_restart"`
	Downloading bool   `json:"downloading,omitempty"`
}

func (s *Server) handleInferencePreferredModel(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, errorBody("method_not_allowed", "POST only"))
		return
	}
	if s.inference == nil || s.catalog == nil || s.catalog.PreferencePath == "" {
		http.Error(w, "catalog not configured", http.StatusNotFound)
		return
	}
	var req PreferredModelRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || (req.ModelID == "" && !req.None) {
		writeJSON(w, http.StatusBadRequest, errorBody("bad_request", `body must be {"model_id":"..."} or {"none":true}`))
		return
	}
	if req.None {
		if req.ModelID != "" {
			writeJSON(w, http.StatusBadRequest, errorBody("bad_request",
				`{"none":true} cannot also name a model_id`))
			return
		}
		s.handleNoModelSelected(w)
		return
	}

	// Resolve against every shipped model, not just the offered ones:
	// this endpoint answers "the operator named THIS model", which is
	// exactly the case a withheld entry has to keep serving.
	manifests, err := s.loadManifestsForResolve()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errorBody("manifest_load_failed", err.Error()))
		return
	}
	manifest, ok := findManifest(manifests, req.ModelID)
	if !ok {
		// A name we WITHDREW gets a named answer, not "never heard of it"
		// (#200). This is the one place the retirement map deliberately
		// does NOT substitute: the handler echoes model_id back and
		// SavePreference persists it, so substituting would write a pin the
		// operator never chose and report an id they never asked for. The
		// tray builds its menu from the offered catalog and so cannot reach
		// this branch at all — what does is a stale tab, a script, or an
		// `init` replaying an old value, and each of those wants to be told.
		//
		// 409, not 404: the request is well-formed and names something real,
		// it is the state of the world that has moved. Same fork as
		// signer.IsRetiredIntegrationTarget, whose other half — a stored
		// control-plane row — is migrated rather than refused, by
		// setupCanonicalModelID.
		if ret, retired := catalog.LookupRetirement(req.ModelID); retired {
			writeJSON(w, http.StatusConflict, errorBody("model_retired",
				fmt.Sprintf("%q was retired; use %q instead", req.ModelID, ret.SuccessorModelID)))
			return
		}
		writeJSON(w, http.StatusNotFound, errorBody("model_not_found", "no bundled manifest with that model_id"))
		return
	}

	if err := agentconfig.SavePreference(s.catalog.PreferencePath, agentconfig.Preference{
		ModelID: req.ModelID,
	}); err != nil {
		writeJSON(w, http.StatusInternalServerError, errorBody("preference_save_failed", err.Error()))
		return
	}

	// #812: apply the switch in process — no whole-agent restart, so the
	// management API, gateway, and mesh all stay up. On any error (a
	// cross-engine target, a not-yet-enrolled daemon, or a wedged setup) fall
	// through to the supervised restart, which re-reads the just-saved
	// preference on boot. Older/unwired daemons (nil hook) take the restart
	// path too. When applying in process the swap layer owns the pull, so the
	// #774 "don't pull pre-restart" reasoning below does not apply.
	if s.catalog.ApplyModelSwitch != nil {
		downloading, err := s.catalog.ApplyModelSwitch(r.Context(), req.ModelID)
		switch {
		case err == nil:
			writeJSON(w, http.StatusAccepted, PreferredModelResponse{
				ModelID:     req.ModelID,
				WillRestart: false,
				Downloading: downloading,
			})
			return
		case errors.Is(err, ErrModelSwitchUnavailable):
			// The host declined to fetch the weights. Restarting would
			// take the whole agent down to re-run a bootstrap that fails
			// the same way, and answering 202 — which is what this did
			// before the swap layer reported the refusal at all — told
			// the operator a switch had happened when nothing had
			// (waired-agent#257).
			//
			// The preference saved above is deliberately kept: it is the
			// choice the operator stated, and it applies by itself once
			// pulls are possible again. The restart path has always left
			// it behind for the same reason.
			writeJSON(w, http.StatusConflict,
				errorBody("model_switch_unavailable", err.Error()))
			return
		}
	}

	// Restart fallback. Downloading only reports whether the chosen family
	// still needs a pull; the pull itself is NOT dispatched here. The imminent
	// restart (scheduled below) would cancel an in-flight request-scoped pull
	// within milliseconds anyway, and its failure path would write a transient
	// failed state a watching client (waired#774) could misread as terminal.
	// The post-restart bootstrap (bootstrapPreferredModel, issue #347)
	// performs the real pull and activates the model once it is ready — the
	// old model keeps serving in the meantime.
	downloading := !modelDownloaded(s.inference.ListModels(r.Context()), manifest.ModelID)

	scheduler := s.catalog.RestartScheduler
	if scheduler == nil {
		scheduler = DefaultRestartScheduler
	}
	go scheduler()

	writeJSON(w, http.StatusAccepted, PreferredModelResponse{
		ModelID:     req.ModelID,
		WillRestart: true,
		Downloading: downloading,
	})
}

// handleNoModelSelected is the {"none":true} arm of /preferred-model
// (waired-agent#586): persist "the operator chose to run without a local
// model", tell the provider so a held fallback download stands down, and
// answer 202 with no model named. No restart is scheduled and no engine
// work happens — there is nothing to apply beyond the record itself; the
// engine keeps running and a model can be chosen later through this same
// endpoint.
func (s *Server) handleNoModelSelected(w http.ResponseWriter) {
	if err := agentconfig.SavePreference(s.catalog.PreferencePath, agentconfig.Preference{
		None: true,
	}); err != nil {
		writeJSON(w, http.StatusInternalServerError, errorBody("preference_save_failed", err.Error()))
		return
	}
	if s.catalog.ApplyNoModelSelected != nil {
		s.catalog.ApplyNoModelSelected()
	}
	writeJSON(w, http.StatusAccepted, PreferredModelResponse{})
}

// ModelChoicePendingRequest is the body of
// POST /waired/v1/inference/model-choice-pending — see
// CatalogConfig.NoteModelChoicePending for what the claim means.
type ModelChoicePendingRequest struct {
	Pending bool `json:"pending"`
}

func (s *Server) handleModelChoicePending(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, errorBody("method_not_allowed", "POST only"))
		return
	}
	if s.inference == nil || s.catalog == nil || s.catalog.NoteModelChoicePending == nil {
		http.Error(w, "model choice claim not supported", http.StatusNotFound)
		return
	}
	var req ModelChoicePendingRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, errorBody("bad_request", `body must be {"pending":true|false}`))
		return
	}
	s.catalog.NoteModelChoicePending(req.Pending)
	w.WriteHeader(http.StatusNoContent)
}

func findManifest(manifests []catalog.Manifest, modelID string) (catalog.Manifest, bool) {
	for _, m := range manifests {
		if m.ModelID == modelID {
			return m, true
		}
	}
	return catalog.Manifest{}, false
}

func modelDownloaded(models []ModelEntry, modelID string) bool {
	for _, m := range models {
		if m.ModelID == modelID && m.State == catalog.ModelStateReady {
			return true
		}
	}
	return false
}

// DefaultRestartScheduler asks the OS service manager to restart the
// agent so the freshly-written preferred-model.json takes effect on
// next boot. The actual mechanism is OS-specific: on Unix we SIGTERM
// our own pid and cmd/waired-agent exits 17, which the systemd unit
// force-restarts (RestartForceExitStatus=17, issue #347); on Windows
// we os.Exit(1) and rely on the SCM Recovery Actions configured at
// service install time. Both paths assume the agent is supervised —
// running waired-agent under nohup will simply terminate the daemon.
// Implementation lives in restart_unix.go / restart_windows.go.

package management

import (
	"encoding/json"
	"net/http"

	"github.com/waired-ai/waired-agent/internal/agentconfig"
	"github.com/waired-ai/waired-agent/internal/catalog"
)

// PreferredModelRequest is the body of POST /waired/v1/inference/preferred-model.
type PreferredModelRequest struct {
	ModelID string `json:"model_id"`
}

// PreferredModelResponse is the 202-Accepted body. WillRestart is always
// true on success; it exists as a wire field so a future Step 12
// hot-swap path can flip it to false without breaking the tray.
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
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.ModelID == "" {
		writeJSON(w, http.StatusBadRequest, errorBody("bad_request", `body must be {"model_id":"..."}`))
		return
	}

	manifests, err := s.loadManifests()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errorBody("manifest_load_failed", err.Error()))
		return
	}
	manifest, ok := findManifest(manifests, req.ModelID)
	if !ok {
		writeJSON(w, http.StatusNotFound, errorBody("model_not_found", "no bundled manifest with that model_id"))
		return
	}

	if err := agentconfig.SavePreference(s.catalog.PreferencePath, agentconfig.Preference{
		ModelID: req.ModelID,
	}); err != nil {
		writeJSON(w, http.StatusInternalServerError, errorBody("preference_save_failed", err.Error()))
		return
	}

	// Kick off a background pull when the chosen family is not yet on
	// disk. Best-effort: we ignore the returned PullJob because the
	// catalog endpoint will surface the in-progress state on the next
	// poll. Errors here should not block the preference write — the
	// agent can retry the pull on next start.
	downloading := false
	if !modelDownloaded(s.inference.ListModels(r.Context()), manifest.ModelID) {
		if _, err := s.inference.PullModel(r.Context(), manifest.ModelID); err == nil {
			downloading = true
		}
	}

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

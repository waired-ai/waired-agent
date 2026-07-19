package management

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/waired-ai/waired-agent/internal/agentconfig"
)

// PublicShareWarningVersion is the version of the consent warning text
// below. Bump it whenever PublicShareWarningText changes in substance —
// stored consent records pin the version they accepted, and a bump
// forces re-consent before public use can resume (spec §11).
const PublicShareWarningVersion = 1

// Public Share first-use warning (spec §14, owner-approved 20260719).
// This is DATA, not copy to edit: the management API is the single
// source for CLI (terminal y/N prompt) and Tray (dialog), so the exact
// wording ships from here. Plain English only; the final "More:" line
// is part of the text body (CLI prints the URL, Tray links it).
const (
	PublicShareWarningTitle = "Use public shared nodes?"

	PublicShareWarningText = `Public nodes are other people's computers. The owner of that computer
could see what you send. Do not send secrets or private data while
using public nodes. The other side may also see your IP address.
Waired records how much you use — never what you send — under a
nickname.

To use public nodes, you must also share one of yours.

More: docs.waired.ai/public-share`

	PublicShareWarningAcceptLabel = "OK — share my machine and start"
	PublicShareWarningCancelLabel = "Cancel"
)

// PublicUseConfig wires the consumer-side Public Share endpoints.
// Path points at public_use.json (agentconfig.DefaultPublicUsePath in
// production); empty disables the routes.
type PublicUseConfig struct {
	Path string
}

// WithPublicUse attaches the consumer-side Public Share settings +
// consent endpoints (/waired/v1/public/*). Pass nil to disable.
// Returns the receiver for chaining.
func (s *Server) WithPublicUse(c *PublicUseConfig) *Server {
	s.publicUse = c
	return s
}

// PublicWarningResponse is the body of GET /waired/v1/public/warning —
// the single source of the consent warning for every UI surface.
type PublicWarningResponse struct {
	Version     int    `json:"version"`
	Title       string `json:"title"`
	Text        string `json:"text"`
	AcceptLabel string `json:"accept_label"`
	CancelLabel string `json:"cancel_label"`
}

func (s *Server) handlePublicWarning(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, errorBody("method_not_allowed", "GET only"))
		return
	}
	writeJSON(w, http.StatusOK, PublicWarningResponse{
		Version:     PublicShareWarningVersion,
		Title:       PublicShareWarningTitle,
		Text:        PublicShareWarningText,
		AcceptLabel: PublicShareWarningAcceptLabel,
		CancelLabel: PublicShareWarningCancelLabel,
	})
}

// PublicUseResponse is the body of GET /waired/v1/public/use and the
// success body of the POST variants. EffectiveMode is what the router
// enforces (off until consent for the current warning version exists).
type PublicUseResponse struct {
	Mode           string `json:"mode"`
	EffectiveMode  string `json:"effective_mode"`
	MinQualityTier int    `json:"min_quality_tier"`
	Main           bool   `json:"main"`
	Sub            bool   `json:"sub"`
	Consented      bool   `json:"consented"`
	WarningVersion int    `json:"warning_version"` // current served version, not the accepted one
}

// PublicUseUpdateRequest is the body of POST /waired/v1/public/use.
// Pointer fields distinguish "leave unchanged" from an explicit value.
type PublicUseUpdateRequest struct {
	Mode           *string `json:"mode,omitempty"`
	MinQualityTier *int    `json:"min_quality_tier,omitempty"`
	Main           *bool   `json:"main,omitempty"`
	Sub            *bool   `json:"sub,omitempty"`
}

// PublicConsentRequest is the body of POST /waired/v1/public/consent.
// WarningVersion must match the currently served version — a client
// that displayed stale text may not record consent for the new one.
type PublicConsentRequest struct {
	WarningVersion int `json:"warning_version"`
}

func (s *Server) publicUseResponse(p agentconfig.PublicUse) PublicUseResponse {
	mode := p.Mode
	if mode == "" {
		mode = agentconfig.PublicUseModeOff
	}
	return PublicUseResponse{
		Mode:           mode,
		EffectiveMode:  p.EffectiveMode(PublicShareWarningVersion),
		MinQualityTier: p.MinQualityTier,
		Main:           p.Main,
		Sub:            p.Sub,
		Consented:      p.Consented(PublicShareWarningVersion),
		WarningVersion: PublicShareWarningVersion,
	}
}

func (s *Server) handlePublicUse(w http.ResponseWriter, r *http.Request) {
	if s.publicUse == nil || s.publicUse.Path == "" {
		http.Error(w, "public use not configured", http.StatusNotFound)
		return
	}
	switch r.Method {
	case http.MethodGet:
		p, _, err := agentconfig.LoadPublicUse(s.publicUse.Path)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, errorBody("public_use_load_failed", err.Error()))
			return
		}
		writeJSON(w, http.StatusOK, s.publicUseResponse(p))
	case http.MethodPost:
		var req PublicUseUpdateRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, errorBody("bad_request", "body must be JSON"))
			return
		}
		p, _, err := agentconfig.LoadPublicUse(s.publicUse.Path)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, errorBody("public_use_load_failed", err.Error()))
			return
		}
		if !p.Consented(PublicShareWarningVersion) {
			// Settings before consent have nothing to act on — the
			// router keeps public candidates off regardless (§4.2).
			writeJSON(w, http.StatusConflict, errorBody("consent_required",
				"accept the current warning first (POST /waired/v1/public/consent)"))
			return
		}
		if req.Mode != nil {
			if !agentconfig.ValidPublicUseMode(*req.Mode) {
				writeJSON(w, http.StatusBadRequest, errorBody("bad_request", "mode must be off|auto|explicit"))
				return
			}
			p.Mode = *req.Mode
		}
		if req.MinQualityTier != nil {
			if *req.MinQualityTier < 0 {
				writeJSON(w, http.StatusBadRequest, errorBody("bad_request", "min_quality_tier must be >= 0"))
				return
			}
			p.MinQualityTier = *req.MinQualityTier
		}
		if req.Main != nil {
			p.Main = *req.Main
		}
		if req.Sub != nil {
			p.Sub = *req.Sub
		}
		if err := agentconfig.SavePublicUse(s.publicUse.Path, p); err != nil {
			writeJSON(w, http.StatusInternalServerError, errorBody("public_use_save_failed", err.Error()))
			return
		}
		writeJSON(w, http.StatusOK, s.publicUseResponse(p))
	default:
		writeJSON(w, http.StatusMethodNotAllowed, errorBody("method_not_allowed", "GET or POST"))
	}
}

func (s *Server) handlePublicConsent(w http.ResponseWriter, r *http.Request) {
	if s.publicUse == nil || s.publicUse.Path == "" {
		http.Error(w, "public use not configured", http.StatusNotFound)
		return
	}
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, errorBody("method_not_allowed", "POST only"))
		return
	}
	var req PublicConsentRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, errorBody("bad_request", `body must be {"warning_version":N}`))
		return
	}
	if req.WarningVersion != PublicShareWarningVersion {
		// The client displayed a different text than the one being
		// consented to — make it re-fetch and re-display.
		writeJSON(w, http.StatusConflict, errorBody("warning_version_mismatch",
			"re-fetch GET /waired/v1/public/warning and show the current text"))
		return
	}
	p, _, err := agentconfig.LoadPublicUse(s.publicUse.Path)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errorBody("public_use_load_failed", err.Error()))
		return
	}
	first := p.Consent == nil
	p.Consent = &agentconfig.PublicConsent{
		AcceptedAt:     time.Now().UTC(),
		WarningVersion: PublicShareWarningVersion,
	}
	if first {
		// §4.2: consent switches auto mode on with both classes enabled
		// and no tier threshold. Re-consent after a warning-text bump
		// keeps whatever the user had configured.
		p.Mode = agentconfig.PublicUseModeAuto
		p.Main = true
		p.Sub = true
		p.MinQualityTier = 0
	}
	if err := agentconfig.SavePublicUse(s.publicUse.Path, p); err != nil {
		writeJSON(w, http.StatusInternalServerError, errorBody("public_use_save_failed", err.Error()))
		return
	}
	writeJSON(w, http.StatusOK, s.publicUseResponse(p))
}

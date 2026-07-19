package intercept

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Display names for the reserved /model route-directive ids (#52). The gateway
// advertises the same id → display-name pairs when it serves /v1/models locally
// (internal/gateway/anthropic_models.go, ModelWaired{Local,Cloud} + the inline
// "Waired local/cloud" strings). They are duplicated here — not shared — so this
// fail-open package stays stdlib-only; keep both sides in sync. The ids
// themselves live in model_rewrite.go (wired{Local,Cloud}Model). id parity is
// asserted by directive_sync_test.go; the display strings are cosmetic (a drift
// only mislabels the picker entry, it still forces the route).
const (
	wairedLocalDisplay = "Waired local (this device)"
	wairedCloudDisplay = "Waired cloud (Anthropic API)"
)

// directiveEntry is one advertised /model directive: its id (for the
// idempotency check) alongside the JSON object served in /v1/models.
type directiveEntry struct {
	id  string
	obj map[string]string
}

// directiveModelEntries builds the two directive model objects in picker order
// (local first, cloud second — matching the local-serving path). The shape
// mirrors gateway.anthropicModel's JSON: {type, id, display_name, created_at}.
// max_input_tokens is intentionally omitted — Claude Code sizes the window from
// the id string plus CLAUDE_CODE_MAX_CONTEXT_TOKENS, not this field.
func directiveModelEntries() []directiveEntry {
	created := time.Now().UTC().Format(time.RFC3339)
	return []directiveEntry{
		{id: wairedLocalModel, obj: map[string]string{
			"type": "model", "id": wairedLocalModel,
			"display_name": wairedLocalDisplay, "created_at": created,
		}},
		{id: wairedCloudModel, obj: map[string]string{
			"type": "model", "id": wairedCloudModel,
			"display_name": wairedCloudDisplay, "created_at": created,
		}},
	}
}

// directiveDisplayName returns the display name for a directive id, or "".
func directiveDisplayName(id string) string {
	switch id {
	case wairedLocalModel:
		return wairedLocalDisplay
	case wairedCloudModel:
		return wairedCloudDisplay
	default:
		return ""
	}
}

// passthroughModels handles GET /v1/models(/{id}) on a passthrough leg (route
// anthropic, auto+degraded, or waired without a local handler). With the #52
// directives feature off it is a plain passthrough. With it on, the reserved
// directive ids are made discoverable even though the real Anthropic model list
// does not carry them:
//
//   - collection form → the upstream list is spliced (the two directive entries
//     prepended to `data`) via a per-request ModifyResponse, so they appear in
//     Claude Code's /model picker even on the anthropic route. This is the whole
//     point: the picker is populated once at startup, and only this makes the
//     "switch back to Waired from /model while on anthropic" path possible.
//   - single-object form for a directive id → synthesized locally, because the
//     real API would 404 an id it has never heard of.
//
// Every other single-object id passes straight through. The merge is fail-open:
// any non-2xx / non-JSON / compressed / unparseable body is forwarded verbatim.
func (s *Server) passthroughModels(w http.ResponseWriter, r *http.Request) {
	if !s.cfg.ModelRouteDirectives {
		s.passthrough(w, r)
		return
	}
	if id, ok := strings.CutPrefix(r.URL.Path, "/v1/models/"); ok && id != "" {
		if isDirectiveModel(id) {
			writeDirectiveModelObject(w, id)
			return
		}
		s.passthrough(w, r)
		return
	}
	// Collection form: splice the directive ids into the real Anthropic list.
	// Only the per-request ReverseProxy copy gets ModifyResponse — never the
	// shared s.rp (same pattern as passthroughWithNotice).
	rp := *s.rp
	rp.ModifyResponse = injectModelDirectives
	rp.ServeHTTP(w, r)
}

// writeDirectiveModelObject answers GET /v1/models/{directive-id} locally with
// the synthesized model object (the real API has no such model).
func writeDirectiveModelObject(w http.ResponseWriter, id string) {
	obj := map[string]string{
		"type": "model", "id": id,
		"display_name": directiveDisplayName(id),
		"created_at":   time.Now().UTC().Format(time.RFC3339),
	}
	b, err := json.Marshal(obj)
	if err != nil {
		http.Error(w, `{"type":"error","error":{"type":"api_error","message":"encode failed"}}`, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(b)
}

// injectModelDirectives is the ModifyResponse hook that prepends the #52
// directive entries into an upstream /v1/models collection response. It is
// fail-open: on any condition it cannot safely handle it restores the original
// body and returns nil so the passthrough is byte-identical to upstream.
func injectModelDirectives(resp *http.Response) error {
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil
	}
	if !strings.Contains(resp.Header.Get("Content-Type"), "json") {
		return nil
	}
	// The passthrough transport sets no Accept-Encoding, so net/http
	// transparently de-gzips and strips Content-Encoding before we see the
	// body. If some encoding is nonetheless present, do not risk corrupting a
	// compressed body — forward it untouched (the picker just won't gain the
	// entries in that rare case).
	if resp.Header.Get("Content-Encoding") != "" {
		return nil
	}
	orig, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		resp.Body = io.NopCloser(bytes.NewReader(orig))
		return nil
	}
	merged, ok := mergeDirectivesIntoModelsJSON(orig)
	if !ok {
		resp.Body = io.NopCloser(bytes.NewReader(orig))
		return nil
	}
	resp.Body = io.NopCloser(bytes.NewReader(merged))
	resp.ContentLength = int64(len(merged))
	resp.Header.Set("Content-Length", strconv.Itoa(len(merged)))
	return nil
}

// mergeDirectivesIntoModelsJSON prepends the directive entries to the `data`
// array of an Anthropic /v1/models envelope, preserving every other field
// (has_more, last_id, and any unknown/future field) byte-for-byte via
// map[string]json.RawMessage. first_id is repointed at the first prepended
// entry. Returns (nil, false) — leave the original alone — when the body is not
// the expected shape or every directive id is already present (idempotent).
func mergeDirectivesIntoModelsJSON(orig []byte) ([]byte, bool) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(orig, &obj); err != nil {
		return nil, false
	}
	rawData, present := obj["data"]
	if !present {
		return nil, false
	}
	var data []json.RawMessage
	if err := json.Unmarshal(rawData, &data); err != nil {
		return nil, false
	}
	existing := map[string]bool{}
	for _, e := range data {
		var m struct {
			ID string `json:"id"`
		}
		if json.Unmarshal(e, &m) == nil && m.ID != "" {
			existing[m.ID] = true
		}
	}
	prepend := make([]json.RawMessage, 0, 2)
	firstID := ""
	for _, entry := range directiveModelEntries() {
		if existing[entry.id] {
			continue
		}
		b, err := json.Marshal(entry.obj)
		if err != nil {
			return nil, false
		}
		if firstID == "" {
			firstID = entry.id
		}
		prepend = append(prepend, b)
	}
	if len(prepend) == 0 {
		return nil, false // all directive ids already present → nothing to do
	}
	newData, err := json.Marshal(append(prepend, data...))
	if err != nil {
		return nil, false
	}
	obj["data"] = newData
	if _, has := obj["first_id"]; has {
		if fid, err := json.Marshal(firstID); err == nil {
			obj["first_id"] = fid
		}
	}
	out, err := json.Marshal(obj)
	if err != nil {
		return nil, false
	}
	return out, true
}

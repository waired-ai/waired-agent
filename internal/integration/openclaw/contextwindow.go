package openclaw

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"
)

// contextWindowTimeout bounds the loopback probe. Apply is interactive — it
// runs inside `waired link` and `waired init` — so a wedged listener on the
// data-plane port must cost a moment, not the whole command.
const contextWindowTimeout = 3 * time.Second

// contextWindowMaxBody caps the /v1/models read. The real body is a few KB;
// the cap only stops a wedged or hostile listener on the port from being
// read without bound.
const contextWindowMaxBody = 1 << 20

// contextWindowFn is the seam Apply calls. Tests replace it to assert the
// wiring; fetchContextWindow itself is exercised directly against an
// httptest server, so replacing it here leaves no implementation untested
// (CLAUDE.md §Test discipline).
var contextWindowFn = fetchContextWindow

// fetchContextWindow asks the data-plane gateway how many input tokens this
// host can actually serve for modelID, reading the max_input_tokens the
// gateway stamps on its /v1/models listing (the same field and source the
// Anthropic listing uses — min of the manifest's native window and the
// tuning the engine really applied, #408).
//
// It returns 0 — never an error — for every way this can come up empty: the
// agent is not running yet (the wizard applies integrations before anything
// serves), the daemon predates the field, no model is active, the body does
// not parse. 0 means "not known", and the plugin then declares no window at
// all rather than a number nothing stands behind. A failure to learn the
// window must not fail a link: the integration works without it.
func fetchContextWindow(ctx context.Context, dataPlaneBaseURL, modelID string) int {
	if dataPlaneBaseURL == "" || modelID == "" {
		return 0
	}
	ctx, cancel := context.WithTimeout(ctx, contextWindowTimeout)
	defer cancel()

	url := strings.TrimRight(dataPlaneBaseURL, "/") + "/v1/models"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, contextWindowMaxBody))
	if err != nil {
		return 0
	}
	return contextWindowFromModels(body, modelID)
}

// contextWindowFromModels picks modelID's max_input_tokens out of an OpenAI
// /v1/models body. Exposed for tests.
func contextWindowFromModels(body []byte, modelID string) int {
	var doc struct {
		Data []struct {
			ID             string `json:"id"`
			MaxInputTokens int    `json:"max_input_tokens"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return 0
	}
	for _, m := range doc.Data {
		if m.ID == modelID && m.MaxInputTokens > 0 {
			return m.MaxInputTokens
		}
	}
	return 0
}

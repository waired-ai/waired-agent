package modelrows

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/waired-ai/waired-agent/internal/integration/claudecode"
	"github.com/waired-ai/waired-agent/proto/hostfit"
)

// fetchTimeout bounds the loopback read. Its callers are interactive — it runs
// inside `waired link` and `waired init` — so a wedged listener on the gateway
// port must cost a moment, not the whole command.
const fetchTimeout = 3 * time.Second

// fetchMaxBody caps the read. The real body is a few KB; the cap only stops a
// wedged or hostile listener on the port from being read without bound.
const fetchMaxBody = 1 << 20

// Fetch asks the Local Gateway which route rows it is offering right now,
// reading the ones GET /v1/models marks `waired_route` (waired-agent#1306).
//
// The gateway is the one place that knows: it holds the mesh snapshot, the
// Public Share posture and whether local inference is on. A caller writing a
// coding tool's model list gets the same rows the Claude picker gets, without
// a second projection.
//
// It returns nothing — never an error — for every way this can come up empty:
// the agent is not running yet (the wizard applies integrations before
// anything serves), the daemon predates the field, the body does not parse. An
// empty answer means "not known", and the caller then writes the one row that
// needs no facts rather than a list nothing stands behind. A failure to learn
// the rows must not fail a link: the integration works without them.
func Fetch(ctx context.Context, gatewayBaseURL string) []Row {
	if gatewayBaseURL == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()

	url := strings.TrimRight(gatewayBaseURL, "/") + "/v1/models"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, fetchMaxBody))
	if err != nil {
		return nil
	}
	return FromModelsBody(body)
}

// FromModelsBody picks the route rows out of an OpenAI /v1/models body.
// Exposed so the decode is testable against a recorded body rather than only
// through a live listener.
//
// Rows the listing does not mark are skipped: the rest of that body is this
// host's model catalog, which is a different question (which model to run)
// from the one these rows answer (which computer runs it).
func FromModelsBody(body []byte) []Row {
	var doc struct {
		Data []struct {
			ID             string `json:"id"`
			WairedRoute    bool   `json:"waired_route"`
			DisplayName    string `json:"display_name"`
			Description    string `json:"description"`
			MaxInputTokens int    `json:"max_input_tokens"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil
	}
	out := make([]Row, 0, len(doc.Data))
	for _, m := range doc.Data {
		if !m.WairedRoute || m.ID == "" {
			continue
		}
		out = append(out, Row{
			DirectiveModel: claudecode.DirectiveModel{
				ID: m.ID, DisplayName: m.DisplayName, Description: m.Description,
			},
			Window1M:      m.MaxInputTokens >= hostfit.ServingWindow1M,
			ContextWindow: m.MaxInputTokens,
		})
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

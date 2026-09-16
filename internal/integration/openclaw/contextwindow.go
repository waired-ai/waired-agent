package openclaw

import "github.com/waired-ai/waired-agent/internal/integration/modelrows"

// rowsFn is the seam Apply, the top-up and the audit use for the route rows
// the gateway is offering. The fetch itself lives in
// internal/integration/modelrows, shared with the Claude picker writer and
// with `waired doctor`, and is exercised there against an httptest server; this
// package only decides when to ask (waired-agent#1306).
var rowsFn = modelrows.Fetch

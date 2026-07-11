// Node preload (via NODE_OPTIONS=--require) that disables HTTP keep-alive on
// the global agents.
//
// Why: firebase-tools authenticates the WIF (external_account) credential by
// POSTing to https://sts.googleapis.com/v1/token via gaxios + node-fetch.
// gaxios only attaches a custom https.Agent for proxy/mTLS, so this request
// falls back to https.globalAgent, which defaults to keepAlive:true in modern
// Node. Node's keep-alive socket-reuse has a regression that surfaces as a
// false "ERR_STREAM_PREMATURE_CLOSE" ("Premature close") on these token
// endpoints (nodejs/node#63989), which firebase-tools masks as
// "Failed to authenticate, have you run firebase login?". auth@v2's Go client
// is unaffected, so only the firebase CLI step needs this.
//
// Disabling keep-alive forces a fresh socket per request (no reuse → no race).
// The token calls are few and short-lived, so the cost is negligible.
const http = require("http");
const https = require("https");
for (const agent of [http.globalAgent, https.globalAgent]) {
  agent.keepAlive = false;
  if (agent.options) agent.options.keepAlive = false;
}

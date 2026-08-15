package main

// declared is the single source of truth for HTTP clients built inside
// cmd/waired. Nothing else clears the guard, so every place this CLI
// decides its own transport is readable in one list — which is exactly
// what was missing when six sites each looked reasonable in its own file
// and all six 403'd against the daemon (#785).
//
// Reason says why the site is NOT a management read. "Talks to something
// other than the local management API" is a good reason. "It only reads
// an allow-listed route" is a weaker one, kept where it is true today and
// marked as such: tcpReadRoutes is the daemon's list, not this CLI's, and
// a route can leave it without anything here failing to compile.
type client struct {
	File   string
	Expr   string
	Reason string
}

const (
	notMgmt    = "not the management API at all"
	mgmtHelper = "the management routing helper itself — this is the client every " +
		"other management call is supposed to reach"
	allowListed = "reads a route inside the daemon's tcpReadRoutes allow-list, so plain " +
		"TCP is served today. Load-bearing on the daemon's list rather than on " +
		"anything here; prefer httpGet when touching this"
)

var declared = []client{
	// The helpers. mgmtReadRoute and mgmtWriteRoute build the client that
	// every other management call is meant to use, and the branch declared
	// here is the deliberate "stay on plain TCP" one — an operator-supplied
	// --mgmt naming some other daemon, which must never be redirected to
	// this machine's socket.
	{"cmd/waired/main.go", "http.Client{}", mgmtHelper},

	// Not the management API.
	{"cmd/waired/infer.go", "http.Client{}", notMgmt + ": the coding-agent data-plane gateway on :9479"},
	{"cmd/waired/update_client.go", "http.Client{}", notMgmt + ": downloads a release asset from GitHub"},
	{"cmd/waired/login_client.go", "http.Client{}", notMgmt + ": the control plane's device-login API"},
	{"cmd/waired/claude.go", "http.Client{}", notMgmt + ": the Anthropic API's /v1/models"},
	{"cmd/waired/init_benchmark.go", "http.Client{}", notMgmt + ": the local inference engine, benchmarked directly"},

	// Allow-listed management reads. These work over plain TCP today
	// because their routes are in tcpReadRoutes.
	{"cmd/waired/doctor.go", "http.Client{}", allowListed + " (/waired/v1/status)"},
	{"cmd/waired/doctor_overlay_key.go", "http.Client{}", allowListed + " (/waired/v1/status)"},
	{"cmd/waired/init_engine_ask.go", "http.Client{}", allowListed + " (/waired/v1/inference/catalog)"},
	{"cmd/waired/models_fit.go", "http.Client{}", allowListed + " (/waired/v1/inference/catalog)"},
	{"cmd/waired/models_catalog.go", "http.Client{}", allowListed + " (/waired/v1/inference/catalog)"},
}

package opencode

import (
	"fmt"
	"os"
	"strings"
	"testing"
)

// TestGatewayBaseURL_UsesTheGatewayItWasGiven replaces a table that pinned
// the opposite: every input, including "http://127.0.0.1:9999", came back
// rewritten to 9479. A host that pinned a non-default gateway port therefore
// got a plugin pointing at a port nothing was listening on, and Audit
// compared it against the same wrong constant so nothing reported it
// (waired-ai/waired-agent#999).
//
// The property now is that the caller's host and port survive. The caller
// resolves them from agent.json (cmd/waired/gatewayurl.go).
func TestGatewayBaseURL_UsesTheGatewayItWasGiven(t *testing.T) {
	cases := map[string]string{
		"http://127.0.0.1:9473":   "http://127.0.0.1:9473",
		"http://127.0.0.1:19473":  "http://127.0.0.1:19473",
		"http://localhost:9473":   "http://localhost:9473",
		"https://127.0.0.1:19473": "https://127.0.0.1:19473",
		// Unusable input still has to produce something dialable.
		"":        "http://127.0.0.1:9473",
		"garbage": "http://127.0.0.1:9473",
	}
	for in, want := range cases {
		if got := GatewayBaseURL(in); got != want {
			t.Errorf("GatewayBaseURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRenderPlugin(t *testing.T) {
	body, err := renderPlugin("http://127.0.0.1:9473")
	if err != nil {
		t.Fatal(err)
	}
	s := string(body)
	for _, want := range []string{
		"export const WairedPlugin",
		"config.provider.waired",
		`"@ai-sdk/openai-compatible"`,
		`baseURL: "http://127.0.0.1:9473/v1"`,
		// The any-computer row, sent as the id Claude Code sends
		// (waired-agent#1395).
		`default: { id: "waired", name: "Waired", limit: limitFor("default") }`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("rendered plugin missing %q:\n%s", want, s)
		}
	}
	// waired/coding and waired/small were retired in waired-agent#521;
	// the restored plugin (waired-agent#982) offers the one surviving
	// alias, the same single entry the OpenClaw plugin carries.
	for _, gone := range []string{`waired/coding`, `waired/small`} {
		if strings.Contains(s, gone) {
			t.Errorf("plugin still offers the retired alias %q", gone)
		}
	}
	// The plugin must not carry a credential: the gateway has none to check
	// (waired-ai/waired#1277).
	if strings.Contains(s, "apiKey") {
		t.Errorf("plugin should not embed an apiKey:\n%s", s)
	}
}

// PIN: product contract — the owner asked for the same choices in OpenCode's
// picker that Claude Code's /model has (rc6 review, waired-ai/waired#1349,
// waired-agent#1306). Reading them at every start rather than baking them in
// is this client's answer to "when do the rows refresh": they name COMPUTERS,
// and which computers are on the mesh changes between links. The `config` hook
// being async and awaited is what makes it possible — measured on OpenCode
// 1.18.30 (2026-09-12).
func TestRenderPlugin_ReadsTheRowsFromTheGateway(t *testing.T) {
	body, err := renderPlugin("http://127.0.0.1:9473")
	if err != nil {
		t.Fatal(err)
	}
	s := string(body)
	for _, want := range []string{
		// The listing, not a baked list.
		`fetch(BASE_URL + "/models"`,
		// Bounded: a wedged listener must cost a moment, not the editor's
		// start-up.
		"AbortSignal.timeout(DISCOVERY_TIMEOUT_MS)",
		// Only the rows that name a computer. The rest of that listing is
		// this host's model catalog, which is a different question — and one
		// of its entries is itself spelled waired/*, so filtering by name
		// would offer a CI fixture model as a computer.
		"m.waired_route",
		// A failed read is "not known": the one row that needs no facts about
		// a mesh is what this integration offered before.
		"|| FALLBACK",
		`default: { id: "waired", name: "Waired", limit: limitFor("default") }`,
		// Each row is sent under its wire id, not the listed one: the
		// any-computer row and its twin are listed as waired/default and
		// waired/default[1m] but carry their floor only as "waired" and
		// "waired[1m]" (waired-agent#1395).
		"id: wire,",
		`return { key: "default", wire: "waired" }`,
		`return { key: "default[1m]", wire: "waired[1m]" }`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("rendered plugin missing %q:\n%s", want, s)
		}
	}
	// The base URL is written twice from one template value. The second
	// spelling is what the installation audit reads, and a plugin from before
	// waired-agent#1306 has only that one
	// (internal/integration/detect/opencode.go).
	if got := strings.Count(s, `"http://127.0.0.1:9473/v1"`); got != 2 {
		t.Errorf("base URL appears %d times, want 2 (const + the audited literal):\n%s", got, s)
	}
}

// TestRenderPlugin_LimitCarriesTheOutputKeyOpenCodeRequires is
// waired-agent#1367. OpenCode's config schema requires `output` wherever
// `limit` appears, and OpenCode 1.18.30 logged a schema rejection on every
// start for a limit carrying `context` alone. Record of today's behaviour:
// 0 is what OpenCode stores when the key is absent, so writing it changes
// nothing but the warning — measured on 1.18.30 (same resolved model, same
// max_tokens on the wire).
func TestRenderPlugin_LimitCarriesTheOutputKeyOpenCodeRequires(t *testing.T) {
	body, err := renderPlugin("http://127.0.0.1:9473")
	if err != nil {
		t.Fatal(err)
	}
	s := string(body)
	// output is always written beside context: 0 on a row at the coding
	// window, a stated limit on a small one (waired-ai/waired#1482).
	if !strings.Contains(s, "let output = 0;") || !strings.Contains(s, "return { context, output };") {
		t.Errorf("rendered plugin does not write output beside context:\n%s", s)
	}
}

// Every Waired row is one of two sessions, decided by its id: 1048576 tokens
// for a "[1m]" row, 200704 for every other, whichever computer answers it. The
// plugin works that out from the key, and the fallback row it offers when the
// gateway cannot be reached carries it too.
//
// PRODUCT CONTRACT, ratifying source: owner decision 2026-09-16 on
// waired-agent#1396. It inverted the listing-driven window of
// waired-agent#1306 / #1395, under which a row whose computer stated no window
// carried none and OpenCode sized it by its own default.
//
// One exception since waired-ai/waired#1481 (from owner rulings 3 and 5 on
// waired-ai/waired#1473, no minimum window for custom models): a row whose
// listing states a window BELOW 200704 — a computer serving a custom model
// that small — takes it, so OpenCode compacts before the gateway refuses a
// turn. The listing is still never read for a larger window.
func TestRenderPlugin_EveryRowsWindowFollowsItsKey(t *testing.T) {
	body, err := renderPlugin("http://127.0.0.1:9473")
	if err != nil {
		t.Fatal(err)
	}
	s := string(body)
	for _, want := range []string{
		"const CONTEXT_WINDOW = 200704;",
		"const CONTEXT_WINDOW_1M = 1048576;",
		`let context = key.toLowerCase().includes("[1m]") ? CONTEXT_WINDOW_1M : CONTEXT_WINDOW;`,
		`if (typeof listed === "number" && listed > 0 && listed < CONTEXT_WINDOW) {`,
		// OpenCode's default output reserve (32,000) would leave a small
		// window less than one request, and it compacted without end
		// (waired-ai/waired#1482, measured with a stub).
		"output = Math.min(SMALL_ROW_OUTPUT, Math.floor(listed / 4));",
		"limit: limitFor(key, m.max_input_tokens),",
		`limit: limitFor("default")`,
		"const PLUGIN_REV = 2;",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("rendered plugin missing %q:\n%s", want, s)
		}
	}
}

// An installed plugin from an older template is rewritten by the refresh that
// runs after link and in doctor's repair; one this build wrote is left alone,
// and so is a home with no plugin. Record of today's behaviour.
func TestTopUpPlugin(t *testing.T) {
	home := t.TempDir()
	if changed, err := TopUpPlugin(home, "http://127.0.0.1:9473"); changed || err != nil {
		t.Fatalf("no plugin: changed=%v err=%v", changed, err)
	}
	if _, err := installPlugin(home, "http://127.0.0.1:9473"); err != nil {
		t.Fatal(err)
	}
	if got := DeclaredRevision(home); got != PluginRevision {
		t.Fatalf("fresh plugin revision %d, want %d", got, PluginRevision)
	}
	if changed, _ := TopUpPlugin(home, "http://127.0.0.1:9473"); changed {
		t.Error("a current plugin was rewritten")
	}
	body, _ := os.ReadFile(PluginFile(home))
	old := strings.Replace(string(body), fmt.Sprintf("const PLUGIN_REV = %d;", PluginRevision), "", 1)
	if err := os.WriteFile(PluginFile(home), []byte(old), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := DeclaredRevision(home); got != 1 {
		t.Fatalf("a plugin without the line reads as revision %d, want 1", got)
	}
	if changed, err := TopUpPlugin(home, "http://127.0.0.1:9473"); !changed || err != nil {
		t.Fatalf("an older plugin: changed=%v err=%v", changed, err)
	}
	if got := DeclaredRevision(home); got != PluginRevision {
		t.Errorf("after the refresh, revision %d, want %d", got, PluginRevision)
	}
}

func TestInstallRemovePlugin(t *testing.T) {
	home := t.TempDir()
	path, err := installPlugin(home, "http://127.0.0.1:9473")
	if err != nil {
		t.Fatal(err)
	}
	if path != PluginFile(home) {
		t.Errorf("installPlugin path = %s, want %s", path, PluginFile(home))
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("plugin not written: %v", err)
	}
	if err := removePlugin(home); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("plugin survived removePlugin")
	}
}

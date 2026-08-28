package gateway

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// A fixture's httptest listener is not private, and neither is
// http.DefaultClient's connection pool. The listener binds a loopback
// ephemeral port every goroutine in the test binary can reach and the
// kernel may re-issue to a later fixture; the shared client pools idle
// connections by host:port across all of them. This package opens more
// than sixty httptest servers in one binary, and its own leg runs them
// under the race detector, which is where waired-agent#1008 was seen: a
// fixture counting "engine attempts" reported five where the gateway can
// issue at most three.
//
// This is the same fix cmd/waired-agent/fixture_traffic_test.go made for
// waired-agent#932 and #933, and it is a copy rather than a shared helper
// only because Go gives test files no visibility across packages. The
// rule it carries is the one that file states: do not try to identify the
// stranger — stamp the traffic the fixture causes, count only that, and
// SAY what else arrived, so the next occurrence names the intruder
// instead of the product.
const fixtureStampHeader = "X-Waired-Test-Fixture"

// fixtureStampSeq distinguishes two live fixtures that share a test name.
var fixtureStampSeq atomic.Uint64

// newFixtureStamp mints a stamp for one fixture.
//
// Minting is separate from applying it because of the order the two ends
// come up in: the listener closes over the stamp, and the client that
// gets stamped usually cannot be built until the listener has an address.
func newFixtureStamp(t *testing.T) string {
	t.Helper()
	return fmt.Sprintf("%s#%d", t.Name(), fixtureStampSeq.Add(1))
}

// stampedClient is the client a fixture hands to Deps.HTTPClient.
//
// It carries the stamp, and — just as load-bearing — it carries its OWN
// Transport, so its idle connections are not pooled with every other
// fixture's. http.DefaultClient's pool is keyed by host:port and outlives
// the server that filled it, so a fixture whose port was previously
// somebody else's can be handed a dead connection; a POST built from a
// bytes.Reader is replayable, and net/http replays it.
func stampedClient(stamp string) *http.Client {
	return &http.Client{Transport: stampingTransport{
		base:  http.DefaultTransport.(*http.Transport).Clone(),
		stamp: stamp,
	}}
}

type stampingTransport struct {
	base  http.RoundTripper
	stamp string
}

func (s stampingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	// Clone: RoundTrip must not mutate the caller's request.
	r = r.Clone(r.Context())
	r.Header.Set(fixtureStampHeader, s.stamp)
	return s.base.RoundTrip(r)
}

// foreignTraffic collects the requests that reached a fixture's listener
// without its stamp. It exists to be printed, not to fail a test: a
// stranger arriving is not the tested code's fault, and turning it into a
// failure would just move the false red somewhere else.
type foreignTraffic struct {
	mu   sync.Mutex
	seen []string
}

// mine reports whether r is this fixture's own traffic, recording it as
// foreign when it is not. A fixture with no stamp (one that has not opted
// in) treats everything as its own, so this is safe to call
// unconditionally.
func (f *foreignTraffic) mine(r *http.Request, stamp string) bool {
	if stamp == "" || r.Header.Get(fixtureStampHeader) == stamp {
		return true
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seen = append(f.seen, fmt.Sprintf("%s %s (stamp=%q ua=%q remote=%s)",
		r.Method, r.URL.Path, r.Header.Get(fixtureStampHeader), r.UserAgent(), r.RemoteAddr))
	return false
}

// report is meant to be interpolated into an assertion's failure message.
func (f *foreignTraffic) report() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.seen) == 0 {
		return "no unstamped requests reached this fixture"
	}
	return fmt.Sprintf("%d unstamped request(s) also reached this fixture — "+
		"they are NOT this subject's and were not counted: %s",
		len(f.seen), strings.Join(f.seen, "; "))
}

// noteForeignTraffic prints the foreign bucket if the test fails for any
// reason, so an unrelated failure in a run that also drew a stranger still
// carries the evidence.
func noteForeignTraffic(t *testing.T, f *foreignTraffic) {
	t.Helper()
	t.Cleanup(func() {
		if t.Failed() {
			t.Log(f.report())
		}
	})
}

// The filter IS the fix, so it is pinned here rather than left to the
// fixture that uses it. Both failure modes are silent from a caller's
// point of view: a filter that admitted everything would leave the test
// exactly as it was, and one that admitted nothing would make it pass
// forever no matter how many times the engine was asked.
func TestFixtureStamp_SeparatesOwnTrafficFromAStranger(t *testing.T) {
	stamp := newFixtureStamp(t)
	var foreign foreignTraffic

	own := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	own.Header.Set(fixtureStampHeader, stamp)
	if !foreign.mine(own, stamp) {
		t.Fatal("the fixture's own stamped request was rejected — a filter that admits " +
			"nothing makes every count assertion vacuous")
	}

	stranger := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	if foreign.mine(stranger, stamp) {
		t.Fatal("an unstamped request was counted as the fixture's own — a filter that " +
			"admits everything leaves waired-agent#1008 exactly where it was")
	}
	wrong := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	wrong.Header.Set(fixtureStampHeader, stamp+"-other-generation")
	if foreign.mine(wrong, stamp) {
		t.Fatal("another generation's stamp was counted as this one's; -count=2 puts two " +
			"generations of every fixture in one process")
	}

	if got := foreign.report(); !strings.Contains(got, "2 unstamped request(s)") ||
		!strings.Contains(got, "/healthz") {
		t.Fatalf("report = %q, want it to name both strangers — the report is the whole "+
			"point of not failing on them", got)
	}

	// The stamp travels on the wire, not just in the struct.
	var sawStamp string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawStamp = r.Header.Get(fixtureStampHeader)
	}))
	t.Cleanup(srv.Close)
	c := stampedClient(stamp)
	resp, err := c.Post(srv.URL, "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if sawStamp != stamp {
		t.Fatalf("server saw stamp %q, want %q — stampedClient is not stamping", sawStamp, stamp)
	}
}

package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/waired-ai/waired-agent/internal/controlclient"
)

// A fixture's httptest listener is not private. It binds a loopback
// ephemeral port that every goroutine in the test binary can reach, and that
// the kernel may re-issue to a later fixture once this one closes. So a
// fixture that records whatever arrives is not observing its subject — it is
// observing the process. Two tests were asserting an absence against exactly
// that and blaming the subject for a stranger's request
// (waired-agent#932, #933), and the seeded-host leg runs `-count=2`, which
// puts two generations of every fixture in one process.
//
// The fix is not to identify the stranger — neither issue could, and a fix
// that depended on it would not be a fix. It is to make each fixture stamp
// the traffic it causes, count only that, and SAY what else arrived, so the
// next occurrence names the intruder instead of the product.

const fixtureStampHeader = "X-Waired-Test-Fixture"

// fixtureStampSeq distinguishes two live fixtures that share a test name,
// which is the ordinary case under `go test -count=2`.
var fixtureStampSeq atomic.Uint64

// newFixtureStamp mints a stamp for one fixture.
//
// Minting is separate from applying it because of the order the two ends
// come up in: the listener closes over the stamp, and the client that gets
// stamped usually cannot be built until the listener has an address. Taking
// the stamp first means the value the handler reads is written before the
// server exists, instead of being assigned underneath a goroutine that is
// already serving — which is a data race, and the kind `go test -race`
// rightly refuses.
func newFixtureStamp(t *testing.T) string {
	t.Helper()
	return fmt.Sprintf("%s#%d", t.Name(), fixtureStampSeq.Add(1))
}

// stampClient makes every request c issues carry stamp.
func stampClient(c *http.Client, stamp string) {
	base := c.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	c.Transport = stampingTransport{base: base, stamp: stamp}
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

// stampedControlClient is a control-plane client whose requests carry
// stamp, and whose idle connections are its own rather than pooled with
// every other fixture's by host:port.
//
// Client.HTTP is exported precisely so a caller can supply its own; the
// production constructor's default is shared, which is what makes a
// fixture's listener reachable by a connection some other fixture opened.
//
// Not separately pinned, because it cannot fail quietly: a client that
// stopped stamping would have every push counted as foreign, and the
// callers' waitUntil for their first push would run out its backstop.
func stampedControlClient(t *testing.T, baseURL, stamp string) *controlclient.Client {
	t.Helper()
	cli := controlclient.NewWithBearer(baseURL, func() string { return "tok" })
	c := &http.Client{Transport: http.DefaultTransport.(*http.Transport).Clone()}
	stampClient(c, stamp)
	cli.HTTP = c
	return cli
}

// foreignTraffic collects the requests that reached a fixture's listener
// without its stamp. It exists to be printed, not to fail a test: a stranger
// arriving is not the tested code's fault, and turning it into a failure
// would just move the false red somewhere else.
type foreignTraffic struct {
	mu   sync.Mutex
	seen []string
}

// mine reports whether r is this fixture's own traffic, recording it as
// foreign when it is not. A fixture with no stamp (one that has not opted in)
// treats everything as its own, so this is safe to call unconditionally.
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

// The filter IS the fix for #932/#933, so it is pinned here rather than left
// to the two fixtures that use it. Both failure modes are silent from a
// caller's point of view: a filter that admitted everything would leave both
// tests exactly as they were, and one that admitted nothing would make them
// pass forever no matter what the subject did.
func TestFixtureStamp_SeparatesOwnTrafficFromAStranger(t *testing.T) {
	stamp := newFixtureStamp(t)
	var foreign foreignTraffic
	var counted atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !foreign.mine(r, stamp) {
			return
		}
		counted.Add(1)
	}))
	t.Cleanup(srv.Close)

	// NB srv.Client() hands out ONE shared client, so stamping it stamps
	// the server's client for good — which is what the fixtures want, and
	// why the stranger below needs a client of its own.
	mine := srv.Client()
	stampClient(mine, stamp)
	resp, err := mine.Get(srv.URL + "/subject")
	if err != nil {
		t.Fatalf("stamped request: %v", err)
	}
	_ = resp.Body.Close()
	if got := counted.Load(); got != 1 {
		t.Fatalf("stamped requests counted = %d, want 1 — the filter admits nothing, "+
			"which makes every assertion built on it vacuous", got)
	}

	// A stranger: the same address, reached by a client this fixture never
	// handed out. This is the traffic that used to be recorded as the
	// subject's.
	resp, err = (&http.Client{}).Get(srv.URL + "/stranger")
	if err != nil {
		t.Fatalf("unstamped request: %v", err)
	}
	_ = resp.Body.Close()
	if got := counted.Load(); got != 1 {
		t.Fatalf("unstamped requests counted = %d, want the count to stay at 1 — "+
			"the filter admits everything, so it is not filtering", got)
	}

	report := foreign.report()
	for _, want := range []string{"1 unstamped request", "GET", "/stranger"} {
		if !strings.Contains(report, want) {
			t.Errorf("foreign report %q does not name %q — the point of recording "+
				"a stranger is that the next occurrence can identify it", report, want)
		}
	}

	// An un-opted-in fixture (no stamp) must keep its old behaviour rather
	// than silently start dropping its own traffic.
	var none foreignTraffic
	if !none.mine(&http.Request{Header: http.Header{}}, "") {
		t.Error("a fixture with no stamp must treat every request as its own")
	}
}

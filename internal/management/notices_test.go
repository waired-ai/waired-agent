package management

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/waired-ai/waired-agent/internal/notice"
)

type stubNotices struct{ ns []notice.Notice }

func (s stubNotices) Notices() []notice.Notice { return s.ns }

func noticesRequest(t *testing.T, srv *Server, method string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(method, "/waired/v1/notices", nil)
	req.RemoteAddr = "127.0.0.1:55555"
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

func TestNoticesEndpointServesWhatTheProviderPublishes(t *testing.T) {
	srv := newServer(Status{}, fakePinger{}).WithNotices(stubNotices{ns: []notice.Notice{
		notice.LighterModel("qwen3-30b-a3b", "qwen3-8b-instruct", 42, 60),
	}})

	rec := noticesRequest(t, srv, http.MethodGet)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}

	// Decode into the wire shape rather than the Go type, so a renamed
	// JSON tag is a failure here rather than a silent break in a CLI
	// built from a different commit.
	var got struct {
		Notices []struct {
			Kind     string `json:"kind"`
			Severity int    `json:"severity"`
			Title    string `json:"title"`
			Text     string `json:"text"`
			Action   int    `json:"action"`
			Target   string `json:"target"`
		} `json:"notices"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Notices) != 1 {
		t.Fatalf("got %d notices, want 1", len(got.Notices))
	}
	n := got.Notices[0]
	if n.Kind != string(notice.KindLighterModel) || n.Title == "" || n.Target != "qwen3-8b-instruct" {
		t.Fatalf("unexpected notice on the wire: %+v", n)
	}
}

func TestNoticesEndpointReturnsAnEmptyListNotNull(t *testing.T) {
	srv := newServer(Status{}, fakePinger{}).WithNotices(stubNotices{})

	rec := noticesRequest(t, srv, http.MethodGet)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d", rec.Code)
	}
	var got NoticesResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Notices == nil {
		t.Fatal("notices came back null; a reader should not have to tell null from empty")
	}
}

// TestNoticesRouteAbsentWithoutProvider
//
// PRODUCT CONTRACT: the nil-dep-disables-the-route convention every
// other optional route in mux() follows. It is what lets a surface use
// 404 to mean "this daemon predates the feature" and render the
// pre-feature view instead of an error.
func TestNoticesRouteAbsentWithoutProvider(t *testing.T) {
	srv := newServer(Status{}, fakePinger{})

	if rec := noticesRequest(t, srv, http.MethodGet); rec.Code != http.StatusNotFound {
		t.Fatalf("code=%d, want 404 so an older-daemon reader can tell", rec.Code)
	}
}

// TestNoticesEndpointIsGETOnly
//
// PRODUCT CONTRACT (waired#836/#838: the loopback guards treat reads and
// writes differently, and a read route that accepts a mutating verb is
// how that separation is lost).
func TestNoticesEndpointIsGETOnly(t *testing.T) {
	srv := newServer(Status{}, fakePinger{}).WithNotices(stubNotices{})

	rec := noticesRequest(t, srv, http.MethodPost)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("code=%d, want 405", rec.Code)
	}
	if allow := rec.Header().Get("Allow"); allow != http.MethodGet {
		t.Errorf("Allow=%q, want GET", allow)
	}
}

// TestNoticesRouteIsSocketOnly
//
// PRODUCT CONTRACT (internal/management/socket.go: "a route added later
// is socket-only until someone adds it here with the consumer that needs
// it", waired#836). All three readers of this route are Go programs that
// already reach the socket, so none of them is that consumer.
func TestNoticesRouteIsSocketOnly(t *testing.T) {
	if _, ok := tcpReadRoutes["/waired/v1/notices"]; ok {
		t.Fatal("/waired/v1/notices is in tcpReadRoutes; it has no non-Go consumer that needs plain TCP")
	}
}

// TestNoticesEndpointClampsALongList records today's behaviour: the
// registry already clamps, and the handler clamps again so a provider
// that grows a bug cannot hand a renderer more rows than it has.
func TestNoticesEndpointClampsALongList(t *testing.T) {
	var many []notice.Notice
	for i := range notice.MaxActive + 4 {
		many = append(many, notice.LighterModel("from", string(rune('a'+i)), 1, 2))
	}
	srv := newServer(Status{}, fakePinger{}).WithNotices(stubNotices{ns: many})

	var got NoticesResponse
	if err := json.Unmarshal(noticesRequest(t, srv, http.MethodGet).Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Notices) != notice.MaxActive {
		t.Fatalf("got %d notices, want %d", len(got.Notices), notice.MaxActive)
	}
}

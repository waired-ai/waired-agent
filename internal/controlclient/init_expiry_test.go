package controlclient

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/waired-ai/waired-agent/internal/devicekeys"
)

// The client half of waired-agent#1175. `waired init` gave up after ten
// minutes with `poll: Get "…": context deadline exceeded` while the
// browser went on promising the setup would carry on by itself.
//
// Two separate faults met there: the client's budget was a second copy of
// the server's TTL, so the server's `expired` answer was unreachable by
// construction; and the budget's context was threaded into the request, so
// it expired MID-REQUEST and surfaced as a transport failure.

// runInitHangGuard bounds each RunInit below so a hung one fails instead of
// hanging. It is not a measurement: no case here asserts how long anything
// took, and every case ends on an answer the rig gives it — the server's
// `expired` / `denied`, or the two-second window it advertises. The number
// only has to be larger than the slowest honest run.
//
// It is one constant, and a generous one, because three of these carried an
// absolute 5 s and one of them ate it: `TestRunInit_Denied` failed at 5.18 s
// on a loaded Windows runner, on the FIRST POST to a loopback httptest
// server, before any polling — and passed on a re-run of the same commit
// (waired-agent#1228). Starvation on a shared runner is additive rather than
// proportional — a runner that stalls this test by 4 s stalls a 30 s budget
// by the same 4 s — so the slack that survives it has to be absolute, not a
// multiple of the fast-machine time.
const runInitHangGuard = 30 * time.Second

func TestPollBudget(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	const fallback = 10 * time.Minute
	rfc := func(d time.Duration) string { return now.Add(d).Format(time.RFC3339) }

	cases := []struct {
		name      string
		expiresAt string
		want      time.Duration
	}{
		// No usable window: an older control plane, or a malformed one.
		// The caller's own value is the fallback, not a guess.
		{"absent", "", fallback},
		{"malformed", "not-a-timestamp", fallback},
		// The window, plus the grace that lets the server's own `expired`
		// answer arrive before this stopwatch fires.
		{"a live window", rfc(30 * time.Minute), 30*time.Minute + expiredPollGrace},
		{"a short window", rfc(2 * time.Minute), 2*time.Minute + expiredPollGrace},
		// expires_at is the SERVER's clock read against ours. A host whose
		// clock is out must not be able to turn the wait into no wait, or
		// into one nobody will sit through.
		{"a window already past", rfc(-time.Hour), minPollBudget},
		{"a window absurdly far off", rfc(72 * time.Hour), maxPollBudget},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := pollBudget(tc.expiresAt, now, fallback); got != tc.want {
				t.Errorf("pollBudget = %v, want %v", got, tc.want)
			}
		})
	}
}

// The ceiling has to clear the server's own window, or a legitimate one
// would be silently truncated and the client would give up early — the
// bug this whole change is about, in a new place.
func TestPollBudgetCeilingClearsTheServersWindow(t *testing.T) {
	if maxPollBudget <= 30*time.Minute+expiredPollGrace {
		t.Errorf("maxPollBudget = %v, too small for a 30-minute LoginSessionTTL", maxPollBudget)
	}
}

// expiryRig is a control plane whose poll answer the test chooses.
type expiryRig struct {
	srv       *httptest.Server
	pollCount int32
	// hold, when non-nil, blocks every poll until it is closed — a
	// request that outlives the budget.
	hold chan struct{}
}

func newExpiryRig(t *testing.T, expiresIn time.Duration, status string) *expiryRig {
	t.Helper()
	rig := &expiryRig{}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/auth/login-sessions", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"login_session_id":      "ls_test",
			"login_url":             "http://placeholder/login/ls_test",
			"user_code":             "TEST-CODE",
			"poll_token":            "waired_poll_test",
			"expires_at":            time.Now().Add(expiresIn).UTC().Format(time.RFC3339),
			"poll_interval_seconds": 1,
		})
	})
	mux.HandleFunc("GET /v1/auth/login-sessions/{id}", func(w http.ResponseWriter, req *http.Request) {
		atomic.AddInt32(&rig.pollCount, 1)
		if rig.hold != nil {
			select {
			case <-rig.hold:
			case <-req.Context().Done():
				return
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"status": status, "reason": "no"})
	})
	rig.srv = httptest.NewServer(mux)
	t.Cleanup(rig.srv.Close)
	return rig
}

// shrinkPollBudgetFloor lets a timeout test exercise a real end-to-end
// expiry in milliseconds. The floor and the grace protect a skewed
// production clock and a slow server answer; neither is worth sitting
// through here, and shrinking them keeps the test on the derivation rather
// than bypassing it.
func shrinkPollBudgetFloor(t *testing.T) {
	t.Helper()
	floor, grace := minPollBudget, expiredPollGrace
	minPollBudget, expiredPollGrace = time.Millisecond, 10*time.Millisecond
	t.Cleanup(func() { minPollBudget, expiredPollGrace = floor, grace })
}

func expiryParams(t *testing.T, url string) InitParams {
	t.Helper()
	dir := t.TempDir()
	mk, err := devicekeys.LoadOrCreateMachineKey(dir + "/machine.key")
	if err != nil {
		t.Fatalf("machine key: %v", err)
	}
	nk, err := devicekeys.LoadOrCreateNodeKey(dir + "/node.key")
	if err != nil {
		t.Fatalf("node key: %v", err)
	}
	return InitParams{
		ControlURL:    url,
		DeviceName:    "test-device",
		Platform:      "linux",
		Arch:          "amd64",
		ClientVersion: "0.1.0-test",
		Endpoint:      "udp4:127.0.0.1:51820",
		MachineKey:    mk,
		NodeKey:       nk,
		PollInterval:  10 * time.Millisecond,
		PollTimeout:   2 * time.Second,
	}
}

// The branch that could never be reached before: the client's budget was
// exactly the server's TTL, so the context always died first.
func TestRunInit_ServerSaysExpired(t *testing.T) {
	rig := newExpiryRig(t, time.Minute, "expired")
	ctx, cancel := context.WithTimeout(context.Background(), runInitHangGuard)
	defer cancel()

	_, err := RunInit(ctx, expiryParams(t, rig.srv.URL))
	if !errors.Is(err, ErrLoginSessionExpired) {
		t.Fatalf("err = %v, want ErrLoginSessionExpired", err)
	}
	if !strings.Contains(err.Error(), "waired init") {
		t.Errorf("the message does not name the command that starts over: %v", err)
	}
}

// And when the server never gets to say it, the ending is the same
// sentence — never the stopwatch's own words.
func TestRunInit_BudgetRunsOut(t *testing.T) {
	shrinkPollBudgetFloor(t)
	// Two seconds, not two milliseconds: expires_at is RFC3339 on the
	// wire, which is second-granular, so a sub-second window truncates to
	// "already past" and the wait ends before a single poll. The grace and
	// the floor are what the helper above shrinks instead.
	rig := newExpiryRig(t, 2*time.Second, "waiting_for_login")
	ctx, cancel := context.WithTimeout(context.Background(), runInitHangGuard)
	defer cancel()

	_, err := RunInit(ctx, expiryParams(t, rig.srv.URL))
	if !errors.Is(err, ErrLoginSessionExpired) {
		t.Fatalf("err = %v, want ErrLoginSessionExpired", err)
	}
	if atomic.LoadInt32(&rig.pollCount) == 0 {
		t.Error("the wait ended before a single poll — the budget was not what ran out")
	}
	if strings.Contains(err.Error(), "context deadline exceeded") {
		t.Errorf("the operator was shown the stopwatch's words: %v", err)
	}
}

// The literal symptom in the issue: the budget expired while a request was
// in flight, and the operator was handed
// `poll: Get "…": context deadline exceeded`.
func TestRunInit_SlowPollDoesNotMasqueradeAsATransportFailure(t *testing.T) {
	shrinkPollBudgetFloor(t)
	// The window has to outlast the first poll interval, or the wait ends
	// at the top of the loop and the in-flight request is never reached —
	// which is the whole thing this test is about. Second granularity on
	// the wire (RFC3339) sets the floor on how short it can be.
	rig := newExpiryRig(t, 2*time.Second, "waiting_for_login")
	rig.hold = make(chan struct{})
	t.Cleanup(func() { close(rig.hold) })

	ctx, cancel := context.WithTimeout(context.Background(), runInitHangGuard)
	defer cancel()

	_, err := RunInit(ctx, expiryParams(t, rig.srv.URL))
	if !errors.Is(err, ErrLoginSessionExpired) {
		t.Fatalf("err = %v, want ErrLoginSessionExpired", err)
	}
	if atomic.LoadInt32(&rig.pollCount) == 0 {
		t.Error("no request was ever in flight — this test proves nothing")
	}
	if strings.Contains(err.Error(), "poll: Get") ||
		strings.Contains(err.Error(), "context deadline exceeded") {
		t.Errorf("a budget that ran out mid-request read as a transport failure: %v", err)
	}
}

// Never tested before either, and it is the other terminal answer.
func TestRunInit_Denied(t *testing.T) {
	rig := newExpiryRig(t, time.Minute, "denied")
	ctx, cancel := context.WithTimeout(context.Background(), runInitHangGuard)
	defer cancel()

	_, err := RunInit(ctx, expiryParams(t, rig.srv.URL))
	if err == nil || !strings.Contains(err.Error(), "login denied") {
		t.Fatalf("err = %v, want a login-denied error", err)
	}
	if errors.Is(err, ErrLoginSessionExpired) {
		t.Errorf("a refusal was reported as an expiry: %v", err)
	}
}

// The window has to reach the caller that owns the terminal's own clock,
// or the CLI goes on carrying a third copy of it.
func TestRunInit_ReportsTheWindow(t *testing.T) {
	rig := newExpiryRig(t, 25*time.Minute, "expired")
	var got time.Time
	p := expiryParams(t, rig.srv.URL)
	p.OnLoginExpiry = func(expiresAt time.Time) { got = expiresAt }

	ctx, cancel := context.WithTimeout(context.Background(), runInitHangGuard)
	defer cancel()
	_, _ = RunInit(ctx, p)

	if got.IsZero() {
		t.Fatal("OnLoginExpiry was never called")
	}
	if d := time.Until(got); d < 20*time.Minute || d > 30*time.Minute {
		t.Errorf("reported window is %v away, want about 25 minutes", d)
	}
}

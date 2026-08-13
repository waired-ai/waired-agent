// Package controlclient implements the agent-side HTTP client for the
// Waired Control Plane. Step3 minimum core: a long-lived Network Map
// subscriber. Step4+ will add the full `waired init` enrollment flow,
// device-token refresh, and Node Key rotation.
package controlclient

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/waired-ai/waired-agent/proto/signer"
)

// Client holds the runtime config for talking to a single Control Plane.
type Client struct {
	BaseURL string
	HTTP    *http.Client

	// BearerFn is invoked on every authenticated request to fetch the
	// current Device Access Token. Plumbed via a closure so the agent's
	// auto-refresh loop can rotate the token without re-creating any
	// of the long-lived consumers (network-map subscription, etc.).
	// Set by New() / NewWithBearer(); never nil after construction.
	BearerFn func() string

	// UseCustomAuthHeader, when true, sends the access token in the
	// X-Waired-Agent-Bearer header instead of Authorization. Set this
	// when the CP is fronted by a Cloud Run / IAP IAM gate that consumes
	// Authorization for its Google identity check; the caller is then
	// responsible for supplying an HTTP client whose transport injects
	// that gate token into Authorization. CP-side fallback lives in
	// internal/controlplane/api.agentBearer.
	UseCustomAuthHeader bool

	// OnboardingCapable declares whether this agent can actually carry
	// out a browser-driven setup — i.e. whether the desired-state applier
	// exists at all (waired-agent#133). It gates the onboarding
	// capabilities in the poll body; see SubscribeNetworkMap.
	//
	// The zero value withholds them. Only the daemon's own subscription
	// declares onboarding, and it says so explicitly.
	OnboardingCapable bool

	// ClientVersion is this build's version, reported on every
	// network-map poll so the control plane's record follows an upgrade
	// (waired-agent#655). It used to travel only in the enrolment
	// payloads, and an installer upgrade restarts the service without
	// re-enrolling — so a device's recorded version froze at whatever
	// first enrolled while it went on polling happily.
	//
	// The poll is the right carrier precisely because it is not a
	// heartbeat: it is opened once per daemon start and once per
	// reconnect, and an upgrade always restarts the daemon.
	//
	// The zero value makes no claim, and the CP leaves whatever it has
	// alone. Same shape as OnboardingCapable: a build-level fact, set by
	// the caller that knows it.
	ClientVersion string
}

// New constructs a Client with a static access token. Use this when
// the token is not expected to rotate (tests, one-shot tools).
func New(baseURL, accessToken string) *Client {
	tok := accessToken
	return NewWithBearer(baseURL, func() string { return tok })
}

// NewWithBearer constructs a Client whose bearer is fetched fresh on
// every authenticated request. Use this in long-running processes
// (`waired-agent`) so the refresh loop can swap the live token under
// active subscribers.
func NewWithBearer(baseURL string, bearerFn func() string) *Client {
	if bearerFn == nil {
		bearerFn = func() string { return "" }
	}
	return &Client{
		BaseURL:  baseURL,
		BearerFn: bearerFn,
		// Long-lived; no Timeout because the network-map stream is a
		// connection that intentionally stays open. Per-call deadlines
		// belong on the request context.
		HTTP: &http.Client{Transport: http.DefaultTransport},
	}
}

// SubscribeNetworkMap opens a long-poll connection to
// POST /v1/network-map/poll and streams NetworkMap frames into the
// returned channel. The caller cancels by cancelling ctx.
//
// On any read error or EOF, the function returns. The returned channel
// is closed before return so callers can `range` over it. Step3 minimum:
// no automatic reconnect - that lives in the agent loop in Step 5.
func (c *Client) SubscribeNetworkMap(ctx context.Context) (<-chan *signer.NetworkMap, <-chan error) {
	frames := make(chan *signer.NetworkMap)
	errs := make(chan error, 1)

	go func() {
		defer close(frames)
		defer close(errs)

		// Declare capabilities (spec §8.4): public-share-v1 tells the
		// CP this agent parses the Public Share map fields (Grant /
		// PublicShare / PublicCapacity), so the CP may emit them and
		// count this device as matchmaking-eligible. onboarding-v1
		// declares the waired#835 desired-state applier below, so the
		// CP may fold desired_engine / desired_model_id /
		// desired_benchmark_gen into this device's own Self entry.
		// onboarding-v2 adds desired_integrations to that set
		// (waired#935): a v1 agent would silently ignore the field and
		// leave the wizard waiting forever for a step it will never
		// emit, so the CP must not send it until an agent says it can
		// apply it. CPs predating the intake ignore the body entirely.
		//
		// The onboarding pair is conditional (waired-agent#133). It used
		// to be unconditional, including on a host with local AI turned
		// off — where the applier is never constructed, so nothing would
		// ever act on the desired state or report a step. The wizard
		// offered a model picker whose confirm button then stayed
		// disabled forever behind a message about the device coming
		// online. Withholding the pair is what lets the CP say the true
		// thing instead (onboarding_reason=unavailable).
		//
		// public-share-v1 stays unconditional, and not only because it is
		// unrelated: the CP tells "declares capabilities, not this one"
		// apart from "polled from a build predating capability intake" by
		// whether the CSV is empty at all, so an empty declaration would
		// have it report an out-of-date client instead.
		//
		// context-window-v1 is unconditional for the same reason: it
		// declares that this BUILD understands
		// InferenceState.ContextWindow on peer entries, which every build
		// carrying this line does. There is nothing to be capable OF at
		// runtime — unlike the onboarding set, whose applier only exists
		// on some paths — and the gate protects signature verification,
		// not the routing decision (waired#1031).
		//
		// ram-available-v1 is unconditional on the same grounds: it
		// declares that this build understands
		// HardwareSummary.RAMAvailableGB on peer entries
		// (waired-agent#568), a build-level fact.
		caps := []string{
			signer.CapabilityContextWindowV1,
			signer.CapabilityRAMAvailableV1,
			// ram-available-v2 rides beside v1, never instead of it: the
			// CP strips BOTH for a poller that declares v2 alone, because
			// a timestamp with no value to date is noise (waired-agent#699).
			// Unconditional for the same reason v1 is — it declares what
			// this BUILD understands on peer entries, not what this host
			// is configured to do.
			signer.CapabilityRAMAvailableV2,
			// vram-free-v1 declares that this build understands
			// HardwareGPUSummary.VRAMFreeMB on peer entries
			// (waired-agent#69) — the per-device free-VRAM reading the
			// ollama budget is sized against. Unconditional for the same
			// reason the two above are: it is a fact about this BUILD's
			// struct, not about this host's configuration. A host whose
			// driver reports no free figure still UNDERSTANDS the field
			// and must keep it byte-identical on re-marshal.
			signer.CapabilityVRAMFreeV1,
			signer.CapabilityPublicShareV1,
		}
		if c.OnboardingCapable {
			// All three or none: the CP gates desired_integrations on v2
			// and desired_model_gen on v3, with the rest on v1, so
			// declaring a later one alone would invite an instruction with
			// no engine or model to go with it. onboarding-v3 is the
			// model-download retry generation (waired-agent#136) — the
			// applier for it is setupReconciler.Apply, which is
			// constructed on exactly this condition.
			// onboarding-v4 is the explicit local-AI answer
			// (waired-agent#597), applied by the same reconciler.
			caps = append(caps,
				signer.CapabilityOnboardingV1,
				signer.CapabilityOnboardingV2,
				signer.CapabilityOnboardingV3,
				signer.CapabilityOnboardingV4,
			)
		}
		// Marshalled rather than concatenated. The capability names are
		// compile-time constants and were safe to splice; ClientVersion is
		// a linker-injected string, and a hand-built body would have to
		// assume nothing in it ever needs escaping.
		//
		// client_version is omitempty because an empty one is "no claim":
		// the CP keeps whatever it already recorded rather than blanking
		// it (waired-agent#655).
		bodyJSON, err := json.Marshal(struct {
			Capabilities  []string `json:"capabilities"`
			ClientVersion string   `json:"client_version,omitempty"`
		}{Capabilities: caps, ClientVersion: c.ClientVersion})
		if err != nil {
			errs <- err
			return
		}
		body := bytes.NewBuffer(bodyJSON)
		req, err := http.NewRequestWithContext(ctx, "POST", c.BaseURL+"/v1/network-map/poll", body)
		if err != nil {
			errs <- err
			return
		}
		req.Header.Set("Content-Type", "application/json")
		bearer := c.BearerFn()
		if c.UseCustomAuthHeader {
			req.Header.Set("X-Waired-Agent-Bearer", bearer)
		} else {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}

		resp, err := c.HTTP.Do(req)
		if err != nil {
			errs <- fmt.Errorf("network-map dial: %w", err)
			return
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			buf, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			errs <- fmt.Errorf("network-map status %d: %s", resp.StatusCode, buf)
			return
		}

		scanner := bufio.NewScanner(resp.Body)
		// Network Map JSON can grow with peers; bump the buffer.
		scanner.Buffer(make([]byte, 64*1024), 8*1024*1024)
		for scanner.Scan() {
			var nm signer.NetworkMap
			if err := json.Unmarshal(scanner.Bytes(), &nm); err != nil {
				errs <- fmt.Errorf("network-map decode: %w", err)
				return
			}
			select {
			case <-ctx.Done():
				return
			case frames <- &nm:
			}
		}
		if err := scanner.Err(); err != nil && !errors.Is(err, context.Canceled) {
			errs <- fmt.Errorf("network-map scan: %w", err)
		}
	}()

	return frames, errs
}

// VerifyMap checks that nm carries a valid signature under the supplied
// CP signing key. Convenience wrapper over signer.VerifyNetworkMap.
func VerifyMap(cpSigningPubB64 string, nm *signer.NetworkMap) error {
	pub, err := decodeBase64(cpSigningPubB64)
	if err != nil {
		return fmt.Errorf("control-plane public key: %w", err)
	}
	if len(pub) != 32 {
		return fmt.Errorf("control-plane public key: expected 32 bytes, got %d", len(pub))
	}
	return signer.VerifyNetworkMap(pub, *nm)
}

// decodeBase64 accepts standard or URL-safe base64 (with or without padding).
func decodeBase64(s string) ([]byte, error) {
	for _, enc := range []*base64.Encoding{
		base64.StdEncoding, base64.URLEncoding,
		base64.RawStdEncoding, base64.RawURLEncoding,
	} {
		if b, err := enc.DecodeString(s); err == nil {
			return b, nil
		}
	}
	return nil, errors.New("base64 (std or url) of an Ed25519 public key")
}

package controlclient

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/waired-ai/waired-agent/internal/devicekeys"
)

// InitParams collects everything `waired init` needs to drive a fresh
// enrollment against a Control Plane.
type InitParams struct {
	ControlURL    string
	DeviceName    string // optional; CP defaults to hostname-style fallback
	Platform      string
	Arch          string
	ClientVersion string
	Endpoint      string // mandatory: e.g., "udp4:host:port" the agent listens on

	MachineKey *devicekeys.MachineKey
	NodeKey    *devicekeys.NodeKey

	// OnLoginURL is invoked once the CP has minted the login session.
	// Implementations typically open the URL in a browser; --no-browser
	// callers print it instead. May be nil for fully scripted tests.
	OnLoginURL func(loginURL, userCode string)

	// OnLoginComplete is invoked once, the moment the login session
	// reaches "authorized" (the user finished the browser sign-in) —
	// before device enrollment. Lets the CLI confirm "Signed in as X"
	// right after the browser step instead of leaving a silent wait. May
	// be nil.
	OnLoginComplete func(accountEmail, networkName string)

	// PollInterval overrides the server-suggested cadence; defaults to
	// 1s for snappy local-dev feedback.
	PollInterval time.Duration

	// PollTimeout caps how long to wait for OAuth completion. Defaults
	// to 10 minutes (matches LoginSession TTL on the server).
	PollTimeout time.Duration

	// HTTPClient overrides the default 30-second-timeout client. Use
	// this to inject a custom transport that adds Authorization headers
	// for IAM-gated upstream services (e.g. a `--bypass-idp` Control
	// Plane on Cloud Run with allow_unauthenticated=false). When set,
	// the poll token is sent in the X-Waired-Poll-Token header so it
	// doesn't collide with the Cloud Run IAM bearer in Authorization.
	HTTPClient *http.Client
}

// InitResult is what RunInit returns on success.
type InitResult struct {
	DeviceID                   string
	NetworkID                  string
	NetworkName                string
	AccountID                  string
	AccountEmail               string
	OverlayIP                  string
	DeviceCertificateJSON      []byte // raw JSON of EnrollDeviceResponse.device_certificate
	DeviceAccessToken          string
	DeviceAccessTokenExpiresAt time.Time
	DeviceRefreshToken         string // empty when talking to a pre-Phase-A CP
	DeviceAuthExpiresAt        time.Time
	NodeKeyExpiresAt           time.Time // zero when talking to a pre-#228 CP
	ControlSigningPublicKey    string    // base64 std
}

// RunInit drives the full Init flow against the Control Plane. The
// caller is responsible for persisting the returned material to disk
// (typically via internal/identity).
func RunInit(ctx context.Context, p InitParams) (*InitResult, error) {
	if p.ControlURL == "" {
		return nil, errors.New("controlclient: ControlURL is required")
	}
	if p.MachineKey == nil || p.NodeKey == nil {
		return nil, errors.New("controlclient: MachineKey and NodeKey are required")
	}
	if p.PollInterval == 0 {
		p.PollInterval = time.Second
	}
	if p.PollTimeout == 0 {
		p.PollTimeout = 10 * time.Minute
	}
	httpClient := p.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}

	clientNonce := make([]byte, 32)
	if _, err := readRandom(clientNonce); err != nil {
		return nil, err
	}

	createBody, _ := json.Marshal(map[string]string{
		"client_version":     p.ClientVersion,
		"device_name":        p.DeviceName,
		"platform":           p.Platform,
		"arch":               p.Arch,
		"machine_public_key": p.MachineKey.PublicBase64(),
		"node_public_key":    p.NodeKey.PublicBase64(),
		"client_nonce":       base64.StdEncoding.EncodeToString(clientNonce),
	})
	create, err := postJSON(ctx, httpClient, p.ControlURL+"/v1/auth/login-sessions", "", createBody)
	if err != nil {
		return nil, fmt.Errorf("create login session: %w", err)
	}
	var createResp struct {
		LoginSessionID string `json:"login_session_id"`
		LoginURL       string `json:"login_url"`
		UserCode       string `json:"user_code"`
		PollToken      string `json:"poll_token"`
		ExpiresAt      string `json:"expires_at"`
	}
	if err := json.Unmarshal(create, &createResp); err != nil {
		return nil, fmt.Errorf("decode create response: %w", err)
	}
	if p.OnLoginURL != nil {
		p.OnLoginURL(createResp.LoginURL, createResp.UserCode)
	}

	pollCtx, cancel := context.WithTimeout(ctx, p.PollTimeout)
	defer cancel()

	var pollResp struct {
		Status             string `json:"status"`
		RegistrationTicket string `json:"registration_ticket"`
		AccountEmail       string `json:"account_email"`
		NetworkID          string `json:"network_id"`
		NetworkName        string `json:"network_name"`
		Reason             string `json:"reason"`
	}
	pollURL := p.ControlURL + "/v1/auth/login-sessions/" + createResp.LoginSessionID
	for {
		select {
		case <-pollCtx.Done():
			return nil, fmt.Errorf("poll timed out waiting for OAuth: %w", pollCtx.Err())
		case <-time.After(p.PollInterval):
		}
		body, err := pollLoginSession(pollCtx, httpClient, pollURL, createResp.PollToken, p.HTTPClient != nil)
		if err != nil {
			return nil, fmt.Errorf("poll: %w", err)
		}
		pollResp = struct {
			Status             string `json:"status"`
			RegistrationTicket string `json:"registration_ticket"`
			AccountEmail       string `json:"account_email"`
			NetworkID          string `json:"network_id"`
			NetworkName        string `json:"network_name"`
			Reason             string `json:"reason"`
		}{}
		if err := json.Unmarshal(body, &pollResp); err != nil {
			return nil, fmt.Errorf("decode poll response: %w", err)
		}
		switch pollResp.Status {
		case "waiting_for_login", "pending_approval":
			continue
		case "authorized":
			if pollResp.RegistrationTicket == "" {
				return nil, errors.New("authorized poll missing registration_ticket")
			}
		case "denied":
			return nil, fmt.Errorf("login denied: %s", pollResp.Reason)
		case "expired":
			return nil, errors.New("login session expired before completion")
		default:
			return nil, fmt.Errorf("unexpected poll status %q", pollResp.Status)
		}
		if pollResp.Status == "authorized" {
			if p.OnLoginComplete != nil {
				p.OnLoginComplete(pollResp.AccountEmail, pollResp.NetworkName)
			}
			break
		}
	}

	// Build & sign the enrollment transcript.
	transcript := machineSignatureTranscript(p.MachineKey.PublicBase64(), p.NodeKey.PublicBase64(), pollResp.RegistrationTicket)
	sig := p.MachineKey.Sign(transcript)

	enrollBody, _ := json.Marshal(map[string]any{
		"registration_ticket": pollResp.RegistrationTicket,
		"machine_public_key":  p.MachineKey.PublicBase64(),
		"node_public_key":     p.NodeKey.PublicBase64(),
		"machine_signature":   base64.StdEncoding.EncodeToString(sig),
		"device_facts": map[string]string{
			"hostname":       p.DeviceName,
			"platform":       p.Platform,
			"arch":           p.Arch,
			"client_version": p.ClientVersion,
			"endpoint":       p.Endpoint,
		},
	})
	enrollResp, err := postJSON(ctx, httpClient, p.ControlURL+"/v1/devices/enroll/complete", "", enrollBody)
	if err != nil {
		return nil, fmt.Errorf("enroll: %w", err)
	}

	var er struct {
		DeviceID                   string          `json:"device_id"`
		NetworkID                  string          `json:"network_id"`
		AccountID                  string          `json:"account_id"`
		OverlayIP                  string          `json:"overlay_ip"`
		DeviceCertificate          json.RawMessage `json:"device_certificate"`
		DeviceAccessToken          string          `json:"device_access_token"`
		DeviceAccessTokenExpiresAt string          `json:"device_access_token_expires_at"`
		DeviceRefreshToken         string          `json:"device_refresh_token"`
		DeviceAuthExpiresAt        string          `json:"device_auth_expires_at"`
		NodeKeyExpiresAt           string          `json:"node_key_expires_at"`
		ControlSigningPublicKey    string          `json:"control_signing_public_key"`
	}
	if err := json.Unmarshal(enrollResp, &er); err != nil {
		return nil, fmt.Errorf("decode enroll response: %w", err)
	}
	atExp, _ := time.Parse(time.RFC3339, er.DeviceAccessTokenExpiresAt)
	authExp, _ := time.Parse(time.RFC3339, er.DeviceAuthExpiresAt)
	nodeExp, _ := time.Parse(time.RFC3339, er.NodeKeyExpiresAt)
	return &InitResult{
		DeviceID:                   er.DeviceID,
		NetworkID:                  er.NetworkID,
		NetworkName:                pollResp.NetworkName,
		AccountID:                  er.AccountID,
		AccountEmail:               pollResp.AccountEmail,
		OverlayIP:                  er.OverlayIP,
		DeviceCertificateJSON:      []byte(er.DeviceCertificate),
		DeviceAccessToken:          er.DeviceAccessToken,
		DeviceAccessTokenExpiresAt: atExp,
		DeviceRefreshToken:         er.DeviceRefreshToken,
		DeviceAuthExpiresAt:        authExp,
		NodeKeyExpiresAt:           nodeExp,
		ControlSigningPublicKey:    er.ControlSigningPublicKey,
	}, nil
}

// machineSignatureTranscript MUST match
// internal/controlplane/api.MachineSignatureTranscript byte-for-byte.
// Duplicated here to avoid an agent -> CP package import.
func machineSignatureTranscript(machinePubB64, nodePubB64, ticket string) []byte {
	var b bytes.Buffer
	b.WriteString("WAIRED-MACHINE-SIGNATURE-V1\n")
	b.WriteString("purpose=device-enrollment\n")
	b.WriteString("machine_public_key=")
	b.WriteString(machinePubB64)
	b.WriteByte('\n')
	b.WriteString("node_public_key=")
	b.WriteString(nodePubB64)
	b.WriteByte('\n')
	b.WriteString("registration_ticket=")
	b.WriteString(ticket)
	b.WriteByte('\n')
	return b.Bytes()
}

// --- HTTP helpers ---

func postJSON(ctx context.Context, c *http.Client, url, bearer string, body []byte) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err := checkHTTPResponse(url, resp, respBody); err != nil {
		return nil, err
	}
	return respBody, nil
}

// pollLoginSession is a GET helper that knows the dual-header convention
// for the poll endpoint. When useCustomHeader is true (caller supplied a
// HTTPClient, i.e. some upstream IAM is occupying Authorization) the
// poll_token rides X-Waired-Poll-Token instead.
func pollLoginSession(ctx context.Context, c *http.Client, url, pollToken string, useCustomHeader bool) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}
	if useCustomHeader {
		req.Header.Set("X-Waired-Poll-Token", pollToken)
	} else {
		req.Header.Set("Authorization", "Bearer "+pollToken)
	}
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err := checkHTTPResponse(url, resp, respBody); err != nil {
		return nil, err
	}
	return respBody, nil
}

// checkHTTPResponse converts a raw Control Plane response into a useful
// error, returning nil when the response is a usable JSON 200. It catches
// the common misconfiguration where --control points at something that is
// not a Waired Control Plane API (a load balancer error page, a web SPA's
// index.html catch-all, a marketing site): such hosts answer with an HTML
// document — frequently even HTTP 200 — which would otherwise surface as a
// cryptic `invalid character '<' looking for beginning of value` JSON
// decode error far from the real cause.
func checkHTTPResponse(url string, resp *http.Response, body []byte) error {
	if looksLikeHTML(resp.Header.Get("Content-Type"), body) {
		ct := resp.Header.Get("Content-Type")
		if ct == "" {
			ct = "unknown"
		}
		return fmt.Errorf("%s returned a non-JSON response (HTTP %d, Content-Type %q) — "+
			"this URL is probably not a Waired Control Plane API endpoint; check that --control "+
			"points at the API host (e.g. https://app.dev.waired.net), not a web page or load balancer",
			url, resp.StatusCode, ct)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status %d: %s", resp.StatusCode, body)
	}
	return nil
}

// looksLikeHTML reports whether contentType/body indicate an HTML document
// rather than a JSON payload. A JSON body never begins with '<', so a
// leading '<' (after trimming leading whitespace) is a reliable tell, as
// is an HTML-ish Content-Type.
func looksLikeHTML(contentType string, body []byte) bool {
	ct := contentType
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = ct[:i]
	}
	switch strings.ToLower(strings.TrimSpace(ct)) {
	case "text/html", "application/xhtml+xml":
		return true
	}
	trimmed := bytes.TrimSpace(body)
	return len(trimmed) > 0 && trimmed[0] == '<'
}

// readRandom abstracts crypto/rand.Read so tests could stub it; today
// it's a thin pass-through.
var readRandom = func(b []byte) (int, error) {
	return cryptoRandRead(b)
}

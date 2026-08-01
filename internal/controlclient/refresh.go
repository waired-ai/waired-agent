package controlclient

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/waired-ai/waired-agent/internal/devicekeys"
)

// RefreshParams is the input to RefreshDeviceToken. Mirrors the wire
// expected by POST /v1/auth/device-token/refresh (spec §12.2).
type RefreshParams struct {
	ControlURL string
	DeviceID   string
	NetworkID  string

	// RefreshToken is the opaque token returned by enrollment or the
	// previous refresh. Hashed locally before being included in the
	// signing transcript.
	RefreshToken string

	// MachineKey signs the transcript so a captured refresh token
	// cannot be used from a host that doesn't also hold the Machine
	// Key private bytes.
	MachineKey *devicekeys.MachineKey

	// ClientNonce pins the per-rotation nonce instead of generating a
	// fresh one. Supplying the SAME nonce on a retry makes the request
	// byte-identical to the attempt whose response was lost, which is
	// what lets the CP tell "this client never heard my answer" apart
	// from "someone replayed a token I already rotated away"
	// (waired-agent#318). Empty → generated here, the one-shot default.
	ClientNonce string

	// HTTPClient is optional; same convention as RunInit — set this
	// when an IAM gate occupies the Authorization header.
	HTTPClient *http.Client
}

// RefreshResult is the decoded server response.
type RefreshResult struct {
	DeviceAccessToken          string
	DeviceAccessTokenExpiresAt time.Time
	DeviceRefreshToken         string
	DeviceAuthExpiresAt        time.Time
	NodeKeyExpiresAt           time.Time
	DeviceCertificateJSON      []byte
}

// Sentinel errors. The agent's refresh loop maps these to behaviour:
// Reused/Unknown/Expired/NotApproved are terminal ("stop trying, surface
// reauth_required and back off"). ErrDeviceSuspended — and any
// unrecognised error — is transient: the loop keeps retrying on its
// backoff, so a suspended device recovers automatically once re-enabled.
//
// ErrRefreshOutcomeUnknown is its own axis, orthogonal to the list
// below: it says the CP's verdict never reached us, so the rotation may
// or may not have committed server-side. It is joined onto the
// transport error rather than replacing it.
var (
	ErrRefreshInvalid       = errors.New("controlclient: refresh token invalid (server: invalid_refresh_token)")
	ErrRefreshExpired       = errors.New("controlclient: refresh token expired (server: expired_refresh_token)")
	ErrRefreshReuseDetected = errors.New("controlclient: refresh token reuse detected — device flipped to reauth_required")
	ErrReauthRequired       = errors.New("controlclient: device authorization or node key expired — reauth required")
	ErrDeviceNotApproved    = errors.New("controlclient: device not in approved state — reauth required")
	ErrDeviceSuspended      = errors.New("controlclient: device suspended — retryable, recovers on enable")
	ErrMachineSigInvalid    = errors.New("controlclient: machine signature did not verify")

	// ErrRefreshOutcomeUnknown marks a rotation whose result the client
	// never learned — the request was written but no usable response
	// came back (connection reset, timeout awaiting headers, truncated
	// body). The stored refresh token may already be burned server-side,
	// so the caller must retry the SAME rotation (same ClientNonce)
	// rather than start a fresh one: a fresh nonce is indistinguishable
	// from token theft and trips reuse detection (waired-agent#318).
	ErrRefreshOutcomeUnknown = errors.New("controlclient: refresh outcome unknown (no response from control plane)")
)

// RefreshDeviceToken posts a refresh request and returns the new
// credentials. The caller is responsible for persisting them on
// success.
func RefreshDeviceToken(ctx context.Context, p RefreshParams) (*RefreshResult, error) {
	if p.ControlURL == "" || p.DeviceID == "" || p.NetworkID == "" || p.RefreshToken == "" {
		return nil, errors.New("controlclient: RefreshDeviceToken: missing required fields")
	}
	if p.MachineKey == nil {
		return nil, errors.New("controlclient: RefreshDeviceToken: MachineKey is required")
	}

	httpClient := p.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}

	nonce := p.ClientNonce
	if nonce == "" {
		nonceBytes := make([]byte, 16)
		if _, err := readRandom(nonceBytes); err != nil {
			return nil, err
		}
		nonce = base64.StdEncoding.EncodeToString(nonceBytes)
	}

	tokenHash := sha256.Sum256([]byte(p.RefreshToken))
	transcript := refreshTranscript(p.DeviceID, p.NetworkID, hex.EncodeToString(tokenHash[:]), nonce)
	sig := p.MachineKey.Sign(transcript)

	body, _ := json.Marshal(map[string]string{
		"device_id":         p.DeviceID,
		"network_id":        p.NetworkID,
		"refresh_token":     p.RefreshToken,
		"machine_signature": base64.StdEncoding.EncodeToString(sig),
		"client_nonce":      nonce,
	})

	req, err := http.NewRequestWithContext(ctx, "POST", p.ControlURL+"/v1/auth/device-token/refresh", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := httpClient.Do(req)
	if err != nil {
		// The request went out; we simply never got a verdict. Flag it
		// so the refresher retries this exact rotation instead of
		// minting a new one (waired-agent#318).
		return nil, errors.Join(ErrRefreshOutcomeUnknown, fmt.Errorf("refresh: %w", err))
	}
	defer resp.Body.Close()
	respBody, rerr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if rerr != nil {
		// Headers arrived but the body did not. On a 200 that means a
		// rotation we cannot decode — same "outcome unknown" position
		// as a transport failure. On an error status the status code
		// alone is a verdict, so classify normally.
		if resp.StatusCode == http.StatusOK {
			return nil, errors.Join(ErrRefreshOutcomeUnknown, fmt.Errorf("refresh: read body: %w", rerr))
		}
		return nil, classifyRefreshError(resp.StatusCode, respBody)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, classifyRefreshError(resp.StatusCode, respBody)
	}

	var rr struct {
		DeviceAccessToken          string          `json:"device_access_token"`
		DeviceAccessTokenExpiresAt string          `json:"device_access_token_expires_at"`
		DeviceRefreshToken         string          `json:"device_refresh_token"`
		DeviceAuthExpiresAt        string          `json:"device_auth_expires_at"`
		NodeKeyExpiresAt           string          `json:"node_key_expires_at"`
		DeviceCertificate          json.RawMessage `json:"device_certificate"`
	}
	if err := json.Unmarshal(respBody, &rr); err != nil {
		// A 200 we cannot decode means the CP rotated but we lost the
		// new credentials — the same predicament as a lost response.
		return nil, errors.Join(ErrRefreshOutcomeUnknown, fmt.Errorf("decode refresh response: %w", err))
	}
	atExp, _ := time.Parse(time.RFC3339, rr.DeviceAccessTokenExpiresAt)
	authExp, _ := time.Parse(time.RFC3339, rr.DeviceAuthExpiresAt)
	nodeExp, _ := time.Parse(time.RFC3339, rr.NodeKeyExpiresAt)
	return &RefreshResult{
		DeviceAccessToken:          rr.DeviceAccessToken,
		DeviceAccessTokenExpiresAt: atExp,
		DeviceRefreshToken:         rr.DeviceRefreshToken,
		DeviceAuthExpiresAt:        authExp,
		NodeKeyExpiresAt:           nodeExp,
		DeviceCertificateJSON:      []byte(rr.DeviceCertificate),
	}, nil
}

// classifyRefreshError maps the CP's structured 401 envelope to one of
// the package-level sentinel errors so the agent can decide
// retry-vs-reauth without parsing JSON itself.
func classifyRefreshError(status int, body []byte) error {
	if status != http.StatusUnauthorized {
		return fmt.Errorf("refresh: status %d: %s", status, body)
	}
	var env struct {
		Error struct {
			Type string `json:"type"`
		} `json:"error"`
	}
	_ = json.Unmarshal(body, &env)
	switch env.Error.Type {
	case "invalid_refresh_token":
		return ErrRefreshInvalid
	case "expired_refresh_token":
		return ErrRefreshExpired
	case "refresh_token_reuse_detected":
		return ErrRefreshReuseDetected
	case "reauth_required":
		return ErrReauthRequired
	case "device_not_approved":
		return ErrDeviceNotApproved
	case "device_suspended":
		return ErrDeviceSuspended
	case "machine_signature_invalid":
		return ErrMachineSigInvalid
	default:
		return fmt.Errorf("refresh: 401 (%s): %s", env.Error.Type, body)
	}
}

// refreshTranscript MUST match internal/controlplane/api.RefreshTranscript
// byte-for-byte. Duplicated here to avoid an agent -> CP package import,
// same as machineSignatureTranscript.
func refreshTranscript(deviceID, networkID, refreshTokenHashHex, clientNonce string) []byte {
	var b bytes.Buffer
	b.WriteString("WAIRED-MACHINE-SIGNATURE-V1\n")
	b.WriteString("purpose=device-token-refresh\n")
	b.WriteString("device_id=")
	b.WriteString(deviceID)
	b.WriteByte('\n')
	b.WriteString("network_id=")
	b.WriteString(networkID)
	b.WriteByte('\n')
	b.WriteString("refresh_token_hash=")
	b.WriteString(refreshTokenHashHex)
	b.WriteByte('\n')
	b.WriteString("client_nonce=")
	b.WriteString(clientNonce)
	b.WriteByte('\n')
	return b.Bytes()
}

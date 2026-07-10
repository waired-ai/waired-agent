package integration

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"

	"github.com/waired-ai/waired-agent/internal/platform/keychain"
	"github.com/waired-ai/waired-agent/internal/platform/securestore"
)

// gatewayTokenLen is the raw byte length of a fresh token. Hex-encoded
// length is 64 chars.
const gatewayTokenLen = 32

// gatewayTokenItem is the Keychain identity for the gateway token. On
// darwin it is mirrored into the macOS Keychain via securestore; the 0600
// file is always kept regardless because env.sh `cat`s it (#261).
func gatewayTokenItem() keychain.Item {
	return keychain.Item{Account: securestore.Account, Service: securestore.ServiceGatewayToken}
}

// LoadOrCreateGatewayToken returns the token at path, generating one
// (32 random bytes hex-encoded, single line, no trailing newline) when
// the file is missing. The file is always written with mode 0600;
// existing files are repermed if they are too loose.
//
// Returned value is the trimmed token string. The on-disk format is
// the hex-encoded ASCII bytes only — no JSON, no envelope — so the
// shell-rc env.sh can `cat` it cheaply.
func LoadOrCreateGatewayToken(path string) (string, error) {
	if path == "" {
		return "", errors.New("integration: empty token path")
	}
	if data, err := securestore.Read(gatewayTokenItem(), path); err == nil {
		token := strings.TrimSpace(string(data))
		if !validGatewayToken(token) {
			return "", fmt.Errorf("integration: %s does not contain a valid gateway token", path)
		}
		// Defensive reperm: a token leaked through a wide mode is
		// treated as a host hygiene issue we silently fix.
		_ = os.Chmod(path, 0o600)
		return token, nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return "", fmt.Errorf("integration: read %s: %w", path, err)
	}
	token, err := generateGatewayToken()
	if err != nil {
		return "", err
	}
	if err := writeTokenFile(path, token); err != nil {
		return "", err
	}
	return token, nil
}

// RotateGatewayToken always regenerates and writes a new token, even
// when the file already exists. Caller is responsible for re-running
// integration.ApplyAll afterwards so env.sh and OpenCode config pick
// up the new value.
func RotateGatewayToken(path string) (string, error) {
	token, err := generateGatewayToken()
	if err != nil {
		return "", err
	}
	if err := writeTokenFile(path, token); err != nil {
		return "", err
	}
	return token, nil
}

func generateGatewayToken() (string, error) {
	buf := make([]byte, gatewayTokenLen)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("integration: token rand: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

func writeTokenFile(path, token string) error {
	if err := securestore.Write(gatewayTokenItem(), path, []byte(token)); err != nil {
		return fmt.Errorf("integration: write %s: %w", path, err)
	}
	return nil
}

func validGatewayToken(s string) bool {
	if len(s) != gatewayTokenLen*2 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}

//go:build darwin

package keychain

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// We shell out to /usr/bin/security rather than linking the Security
// framework via CGO. The CLI is a stable, documented interface (man
// security(1)) shipped with macOS since 10.0, and avoiding CGO keeps
// `CGO_ENABLED=0 GOOS=darwin go vet` working in CI cross-vet — the
// agent binary stays a single static-ish ELF/Mach-O without a libSystem
// link cost that varies across SDKs.
//
// The trade-off is: each operation forks /usr/bin/security. For the
// access patterns we care about (init-time read of a device key, daily
// refresh of an auth token) that is fine; if a hot path ever needs
// per-request lookups we would revisit.

const securityBinary = "/usr/bin/security"

// systemKeychainPath is the root-owned, session-less keychain the
// waired-agent system LaunchDaemon stores its secrets in. A root daemon
// has no login (Aqua) session, so the per-user login keychain is
// unreachable (errSecInteractionNotAllowed); the System keychain is
// always unlocked for root and needs no GUI. See #520 / #515.
const systemKeychainPath = "/Library/Keychains/System.keychain"

// geteuidFn is overridden in tests so the System-keychain routing can be
// exercised on a non-root CI host.
var geteuidFn = os.Geteuid

// useSystemKeychain reports whether secrets should be routed to the
// System keychain instead of the per-user login keychain. The agent
// daemon and `sudo waired init` run as root and target the System
// keychain; user-context callers (the tray's `waired link` writing the
// gateway token) run unprivileged and use the login keychain they own.
func useSystemKeychain() bool { return geteuidFn() == 0 }

// withKeychainTarget appends the explicit System-keychain positional
// argument when running as root, so the item is created in / read from /
// deleted from the System keychain rather than root's (nonexistent)
// login keychain. For the login keychain the positional is omitted and
// `security` uses the caller's default.
func withKeychainTarget(args []string) []string {
	if useSystemKeychain() {
		return append(args, systemKeychainPath)
	}
	return args
}

// secretCodecPrefix tags every value this backend writes to the
// Keychain. We must not hand raw bytes to `security` for two reasons,
// both of which corrupt binary secrets (see #512):
//
//   - `find-generic-password -w` renders a *binary* password back as a
//     hex string, so a stored 64-byte ed25519 key reads back as 128
//     characters and the daemon refuses to load it.
//   - exec argv cannot carry a NUL byte, which an ed25519 private key
//     frequently contains, so the raw `Set` would have failed outright.
//
// encodeSecret therefore base64-encodes the bytes (printable, NUL-free,
// stable under `-w`) behind this prefix. The trailing ':' is deliberate:
// ':' never appears in base64, hex, or JWT/base64url output, so a value
// written by the old raw code (read back by `security -w` as a hex
// string) — or any foreign Keychain item — can never begin with this
// prefix. decodeSecret can thus distinguish "ours" from "legacy/foreign"
// with zero dependence on the exact shape of the CLI's hex rendering.
const secretCodecPrefix = "wkc1:" // waired keychain codec v1

// encodeSecret renders data as the printable token stored via `-w`.
func encodeSecret(data []byte) string {
	return secretCodecPrefix + base64.StdEncoding.EncodeToString(data)
}

// decodeSecret reverses encodeSecret. ok=false means the stored value was
// not written by this backend — a legacy raw-binary item from before the
// codec existed, or a foreign item. The caller treats that as ErrNotFound
// so securestore falls back to the authoritative 0600 file and re-migrates
// it into the new format on the next write.
func decodeSecret(raw []byte) (data []byte, ok bool) {
	s, found := strings.CutPrefix(string(raw), secretCodecPrefix)
	if !found {
		return nil, false
	}
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return nil, false
	}
	return b, true
}

// runSecurityFn is overridden in tests to inject a fake `security`
// implementation that records argv and returns canned output. Default
// is to actually exec the system binary.
var runSecurityFn = runSecurityReal

type darwinStore struct{}

func newStore() Store { return darwinStore{} }

func (darwinStore) Set(item Item, data []byte) error {
	if err := validate(item); err != nil {
		return err
	}
	// -U → update existing item if present (idempotent upsert).
	// -w <password>: pass the secret on argv as a base64 codec token
	// (encodeSecret). The raw bytes must never go on argv directly:
	// `security -w` round-trips binary as hex on read and exec argv
	// cannot carry a NUL — both corrupt binary secrets (#512). (`security`
	// does not expose a stdin-based password flag for generic items — argv
	// it is. The binary is local, args are not logged to syslog by
	// the CLI itself, and the laptop is single-user, so argv leakage
	// is bounded.)
	// -T <path>: add the binary's own path to the item's ACL so
	// subsequent reads from the same binary do not re-prompt. We
	// pass the literal symlink "/usr/bin/security" because we want
	// the CLI itself to retain access (we always go through it).
	// Adding our own binary as well lets future direct-framework
	// callers stay silent.
	args := []string{
		"add-generic-password",
		"-a", item.Account,
		"-s", item.Service,
		"-w", encodeSecret(data),
		"-U",
	}
	if useSystemKeychain() {
		// -A: allow any application to access without an ACL prompt. A
		// headless root daemon cannot answer the partition/ACL dialog the
		// per-binary -T path can raise, and the System keychain item is
		// already gated by root-only file permissions.
		args = append(args, "-A")
	} else {
		args = append(args, "-T", securityBinary)
	}
	args = withKeychainTarget(args)
	stdout, stderr, err := runSecurityFn(args, nil)
	if err != nil {
		return fmt.Errorf("keychain set %s/%s: %w (stdout=%q stderr=%q)",
			item.Account, item.Service, err,
			truncate(stdout), truncate(stderr))
	}
	return nil
}

func (darwinStore) Get(item Item) ([]byte, error) {
	if err := validate(item); err != nil {
		return nil, err
	}
	// -w: print the password (data) only, no metadata. The CLI
	// trailing newline is stripped below.
	args := withKeychainTarget([]string{
		"find-generic-password",
		"-a", item.Account,
		"-s", item.Service,
		"-w",
	})
	stdout, stderr, err := runSecurityFn(args, nil)
	if err != nil {
		if isNotFoundError(stderr) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("keychain get %s/%s: %w (stderr=%q)",
			item.Account, item.Service, err, truncate(stderr))
	}
	// `security ... -w` appends a trailing newline; remove the single
	// terminator the CLI itself adds before decoding.
	out := stdout
	if n := len(out); n > 0 && out[n-1] == '\n' {
		out = out[:n-1]
	}
	// Decode the codec token back to the original bytes. A value without
	// our prefix is a legacy raw-binary item (written before the codec
	// existed, now hex-mangled by `security -w`) or a foreign item; report
	// it as a miss so securestore re-migrates from the authoritative file.
	data, ok := decodeSecret(out)
	if !ok {
		return nil, ErrNotFound
	}
	return data, nil
}

func (darwinStore) Delete(item Item) error {
	if err := validate(item); err != nil {
		return err
	}
	args := withKeychainTarget([]string{
		"delete-generic-password",
		"-a", item.Account,
		"-s", item.Service,
	})
	_, stderr, err := runSecurityFn(args, nil)
	if err != nil {
		if isNotFoundError(stderr) {
			return ErrNotFound
		}
		return fmt.Errorf("keychain delete %s/%s: %w (stderr=%q)",
			item.Account, item.Service, err, truncate(stderr))
	}
	return nil
}

func (s darwinStore) Exists(item Item) (bool, error) {
	if err := validate(item); err != nil {
		return false, err
	}
	// Use find-generic-password without -w (metadata-only) so we do
	// not need the user's permission to read the data — checking
	// presence is allowed even when the item ACL would block reads.
	args := withKeychainTarget([]string{
		"find-generic-password",
		"-a", item.Account,
		"-s", item.Service,
	})
	_, stderr, err := runSecurityFn(args, nil)
	if err != nil {
		if isNotFoundError(stderr) {
			return false, nil
		}
		return false, fmt.Errorf("keychain exists %s/%s: %w (stderr=%q)",
			item.Account, item.Service, err, truncate(stderr))
	}
	return true, nil
}

func validate(item Item) error {
	if item.Account == "" || item.Service == "" {
		return errors.New("keychain: Account and Service must be non-empty")
	}
	return nil
}

// isNotFoundError checks the security(1) stderr for the
// errSecItemNotFound code. The CLI prints:
//
//	security: SecKeychainSearchCopyNext: The specified item could not be found in the keychain.
//
// which carries the constant -25300 internally. We match on the
// human-readable substring rather than the numeric code because the
// CLI does not always include the latter.
func isNotFoundError(stderr []byte) bool {
	s := string(stderr)
	return strings.Contains(s, "could not be found in the keychain") ||
		strings.Contains(s, "-25300")
}

// runSecurityReal is the default runSecurityFn. It forks
// /usr/bin/security with the supplied argv and returns stdout, stderr,
// and the exec error.
func runSecurityReal(args []string, stdin []byte) ([]byte, []byte, error) {
	cmd := exec.Command(securityBinary, args...)
	if len(stdin) > 0 {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return stdout.Bytes(), stderr.Bytes(), err
}

// truncate keeps debug strings from blowing up error messages when
// `security` happens to emit a long stderr (e.g. an ACL listing).
func truncate(b []byte) string {
	const max = 200
	if len(b) <= max {
		return string(b)
	}
	return string(b[:max]) + "…"
}

// Package keychain provides opt-in access to the macOS Keychain for
// callers that want at-rest encryption stronger than POSIX 0o600 file
// mode.
//
// The default secret storage in waired is file-based (see
// internal/platform/secrets, which writes mode 0600 on Unix and applies
// SYSTEM+Administrators+CurrentUser DACLs on Windows). That model is
// sufficient for a single-user developer laptop where the threat is
// "another local user reads the file." It is NOT sufficient against
// "the laptop is stolen, the disk is unlocked or cloned" — at-rest
// data is plaintext.
//
// On macOS the user's login password protects the login Keychain at
// rest. Moving a high-value secret (long-lived auth token, device
// private key) into the Keychain raises the bar to "attacker also
// knows the user's password / has the user's logged-in session." For
// the small inconvenience of the first-access prompt this is a real
// security upgrade.
//
// This package is the API surface that callers can use to opt in. It
// is deliberately separate from internal/platform/secrets because the
// disk-path-keyed semantics there ("WriteSecret(path, data)" + caller
// reads via os.ReadFile) do not map cleanly to a system credential
// store. The Keychain identifies an item by (account, service); the
// caller must know both to read it back. Migrating an existing
// disk-path callsite therefore requires changing both the write site
// and every read site, and that migration is intentionally NOT done
// in this package — it is per-callsite future work tracked in
// docs/todo.md.
//
// On non-darwin platforms every operation returns ErrUnsupported. The
// build tags ensure no host-OS facility is silently used.
package keychain

import "errors"

// ErrUnsupported is returned by every operation on non-darwin
// platforms. Callers should branch on runtime.GOOS == "darwin" before
// calling, or treat ErrUnsupported as "fall back to file-based
// storage."
var ErrUnsupported = errors.New("keychain: only supported on darwin")

// ErrNotFound is returned by Get / Delete when no item with the
// supplied (account, service) tuple exists.
var ErrNotFound = errors.New("keychain: item not found")

// Item identifies an entry in the system credential store. account is
// typically a fixed string per application ("waired") so all items
// group under the same logical owner; service is the per-secret label
// ("device-key", "gateway-token", etc.). Both are required and must
// be non-empty.
type Item struct {
	Account string
	Service string
}

// Store is the platform-specific credential store. Use New() to
// construct one; on non-darwin it returns a stub whose every method
// returns ErrUnsupported.
type Store interface {
	// Set stores data under item, overwriting any existing entry. The
	// first call from a newly-built binary triggers a macOS Keychain
	// authorization prompt; subsequent calls from the same binary
	// path are silent (the binary is added to the item's ACL).
	Set(item Item, data []byte) error

	// Get returns the stored data, or ErrNotFound if no such item.
	// May trigger an authorization prompt if the calling binary is
	// not on the item's ACL.
	Get(item Item) ([]byte, error)

	// Delete removes the item. Returns ErrNotFound if it did not
	// exist; idempotent retries on the same item.
	Delete(item Item) error

	// Exists reports whether an item is present, without retrieving
	// its data. Useful for "should we migrate from file-based
	// storage?" checks.
	Exists(item Item) (bool, error)
}

// New returns the platform-specific Store. On darwin this is a Store
// backed by the `security` CLI; on every other platform it is a stub
// whose methods all return ErrUnsupported.
func New() Store {
	return newStore()
}

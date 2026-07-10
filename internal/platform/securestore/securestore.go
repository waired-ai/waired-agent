// Package securestore composes the opt-in macOS Keychain
// (internal/platform/keychain) with the file-based secret store
// (internal/platform/secrets) so at-rest secret callsites can prefer the
// Keychain on darwin while transparently falling back to a 0600 file on
// every other platform.
//
// The Keychain identifies an item by (account, service); the file store
// is keyed by path. A migrated callsite passes BOTH: the Keychain Item
// and the on-disk fallback path. During the migration window:
//
//   - Read prefers the Keychain. On a genuine Keychain miss it reads the
//     file and, on a Keychain-capable OS, opportunistically migrates the
//     file's bytes into the Keychain so the next read is Keychain-first —
//     this is how secrets created before the migration move across.
//   - Write is a dual-write: the file is authoritative, the Keychain is
//     the at-rest upgrade layered on top.
//   - Remove deletes both, so a logout-style wipe of the file cannot leave
//     a stale Keychain item behind.
//
// Invariant: the Keychain entry is correct-or-absent, never stale relative
// to the file. That makes a Keychain-first Read always safe — a hit is
// current, a miss falls through to the authoritative file.
//
// On non-darwin platforms keychain.New() is the ErrUnsupported stub, so
// every path degrades to pure file behaviour — byte-identical to the
// previous secrets.WriteSecret + os.ReadFile callsites.
package securestore

import (
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"sync"

	"github.com/waired-ai/waired-agent/internal/platform/keychain"
	"github.com/waired-ai/waired-agent/internal/platform/secrets"
)

// Account is the fixed Keychain account string for every waired secret, so
// all items group under one logical owner. Callsites reference it instead
// of typing the literal, which keeps the (account, service) namespace in
// one place.
const Account = "waired"

// Service labels for the migrated secrets. Each must be unique under
// Account — two secrets sharing a service would clobber each other in the
// Keychain. Keeping them here is the single source of truth that prevents
// such a collision.
const (
	ServiceMachineKey   = "machine-key"
	ServiceAccessToken  = "access-token"
	ServiceRefreshToken = "refresh-token"
	ServiceGatewayToken = "gateway-token"
	ServiceCPSignerKey  = "cp-signer-key"
)

var (
	storeMu sync.RWMutex
	store   keychain.Store = keychain.New()
	logger                 = slog.Default()
)

func currentStore() keychain.Store {
	storeMu.RLock()
	defer storeMu.RUnlock()
	return store
}

// Read returns the secret for item, preferring the Keychain and falling
// back to the file at path. When neither source has the secret the
// returned error satisfies os.IsNotExist (it is the file-read error
// verbatim), so callers keep their existing "missing => generate new"
// branch unchanged.
func Read(item keychain.Item, path string) ([]byte, error) {
	st := currentStore()
	data, err := st.Get(item)
	switch {
	case err == nil:
		return data, nil
	case errors.Is(err, keychain.ErrUnsupported):
		// No Keychain on this OS: file is the only store.
		return os.ReadFile(path)
	case errors.Is(err, keychain.ErrNotFound):
		// Genuine miss on a Keychain-capable OS: read the file and
		// migrate it across (handled below).
	default:
		// Locked Keychain, denied ACL, security-CLI failure, etc. The
		// file is authoritative during the migration window, so fall back
		// rather than fail a process whose on-disk secret is intact.
		logger.Warn("securestore: keychain read failed; falling back to file",
			"service", item.Service, "err", err)
		return os.ReadFile(path)
	}

	fileData, ferr := os.ReadFile(path)
	if ferr != nil {
		// Both stores empty (or a real file error): return it verbatim so
		// os.IsNotExist still works for the "generate new" path.
		return nil, ferr
	}
	// Opportunistic migration: copy the pre-existing file secret into the
	// Keychain so future reads are Keychain-first. Best-effort — on failure
	// the Keychain stays empty (not stale) and we still return the file
	// bytes; the next read retries.
	if serr := st.Set(item, fileData); serr != nil && !errors.Is(serr, keychain.ErrUnsupported) {
		logger.Warn("securestore: opportunistic keychain migration failed; using file",
			"service", item.Service, "err", serr)
	}
	return fileData, nil
}

// Write stores data in both the file (authoritative, atomic 0600) and the
// Keychain (best-effort at-rest upgrade). A file-write failure is returned;
// a Keychain failure on darwin is logged, not returned, and any stale
// Keychain entry is cleared so it cannot shadow the freshly-written file.
func Write(item keychain.Item, path string, data []byte) error {
	if err := secrets.WriteSecret(path, data); err != nil {
		return err
	}
	st := currentStore()
	if err := st.Set(item, data); err != nil {
		if errors.Is(err, keychain.ErrUnsupported) {
			return nil // expected steady state off-darwin; not worth logging
		}
		logger.Warn("securestore: keychain write failed; file written, clearing any stale entry",
			"service", item.Service, "err", err)
		// Never-stale invariant: a partial/failed Set must not leave an old
		// value that a Keychain-first Read would prefer over the new file.
		if derr := st.Delete(item); derr != nil &&
			!errors.Is(derr, keychain.ErrNotFound) && !errors.Is(derr, keychain.ErrUnsupported) {
			logger.Warn("securestore: failed to clear stale keychain entry after write failure",
				"service", item.Service, "err", derr)
		}
	}
	return nil
}

// Remove deletes the secret from both stores. A missing file is not an
// error and a file-removal failure is returned (matching the prior
// os.Remove semantics at callsites). Keychain deletion is best-effort: a
// failure is logged loudly — the credential would otherwise linger — but
// does not block the removal, so a logout always clears local state.
func Remove(item keychain.Item, path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err := currentStore().Delete(item); err != nil &&
		!errors.Is(err, keychain.ErrNotFound) && !errors.Is(err, keychain.ErrUnsupported) {
		logger.Warn("securestore: could not delete keychain item; it may linger after removal",
			"service", item.Service, "err", err)
	}
	return nil
}

// SwapStoreForTest replaces the process-wide Keychain backend and returns a
// function that restores the previous one. It lets tests in any package
// inject NewMemStore() so a `go test` run on darwin never execs
// /usr/bin/security or triggers an authorization prompt:
//
//	t.Cleanup(securestore.SwapStoreForTest(securestore.NewMemStore()))
//
// It is exported (rather than living in an _test.go file) precisely so
// black-box test packages — e.g. signer_test — can reach it.
func SwapStoreForTest(s keychain.Store) (restore func()) {
	storeMu.Lock()
	prev := store
	store = s
	storeMu.Unlock()
	return func() {
		storeMu.Lock()
		store = prev
		storeMu.Unlock()
	}
}

// memStore is an in-memory keychain.Store for tests.
type memStore struct {
	mu   sync.Mutex
	data map[keychain.Item][]byte
}

// NewMemStore returns an in-memory keychain.Store for tests. A miss returns
// keychain.ErrNotFound, mirroring the real darwin store so the securestore
// fallback / migration paths behave exactly as in production.
func NewMemStore() keychain.Store {
	return &memStore{data: make(map[keychain.Item][]byte)}
}

func (m *memStore) Set(item keychain.Item, data []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := make([]byte, len(data))
	copy(cp, data)
	m.data[item] = cp
	return nil
}

func (m *memStore) Get(item keychain.Item) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.data[item]
	if !ok {
		return nil, keychain.ErrNotFound
	}
	out := make([]byte, len(v))
	copy(out, v)
	return out, nil
}

func (m *memStore) Delete(item keychain.Item) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.data[item]; !ok {
		return keychain.ErrNotFound
	}
	delete(m.data, item)
	return nil
}

func (m *memStore) Exists(item keychain.Item) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.data[item]
	return ok, nil
}

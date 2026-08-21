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
		logger.Debug("securestore: read served from keychain", "service", item.Service)
		return data, nil
	case errors.Is(err, keychain.ErrUnsupported):
		// No Keychain on this OS: file is the only store.
		logger.Debug("securestore: keychain unsupported; reading from file", "service", item.Service)
		return os.ReadFile(path)
	case errors.Is(err, keychain.ErrNotFound):
		// Genuine miss on a Keychain-capable OS: read the file and
		// migrate it across (handled below).
	default:
		// Denied ACL, security-CLI failure, or simply no session to
		// open the keychain in. The file is authoritative during the
		// migration window, so fall back rather than fail a process
		// whose on-disk secret is intact.
		if keychainMissLevel(err) == slog.LevelDebug {
			logger.Debug("securestore: keychain not reachable from this session; reading the protected file",
				"service", item.Service)
		} else {
			logger.Warn("securestore: keychain read failed; falling back to file",
				"service", item.Service, "err", err)
		}
		return os.ReadFile(path)
	}

	fileData, ferr := os.ReadFile(path)
	if ferr != nil {
		// Both stores empty (or a real file error): return it verbatim so
		// os.IsNotExist still works for the "generate new" path.
		logger.Debug("securestore: secret absent from keychain and file", "service", item.Service)
		return nil, ferr
	}
	// Opportunistic migration: copy the pre-existing file secret into the
	// Keychain so future reads are Keychain-first. Best-effort — on failure
	// the Keychain stays empty (not stale) and we still return the file
	// bytes; the next read retries.
	if serr := st.Set(item, fileData); serr != nil && !errors.Is(serr, keychain.ErrUnsupported) {
		if keychainMissLevel(serr) == slog.LevelDebug {
			logger.Debug("securestore: keychain not reachable from this session; the file remains the only copy",
				"service", item.Service)
		} else {
			logger.Warn("securestore: opportunistic keychain migration failed; using file",
				"service", item.Service, "err", serr)
		}
	}
	logger.Debug("securestore: keychain miss; served from file", "service", item.Service)
	return fileData, nil
}

// writeReport is everything Write says about the Keychain half of a
// dual-write: at what level, in what words, and which error to attach.
//
// It is a value rather than a log call because the defect
// waired-agent#799 records is not what the code DID — the file was
// written, the invariant held — it is what the code SAID about it. So
// the sentence is the unit under test, and classifyWrite is a pure
// function of the two errors that produced it, testable without a
// keychain, a Mac, or a log sink.
type writeReport struct {
	Level slog.Level
	Msg   string
	// Err is nil when nothing failed in a way the reader can act on.
	// A refusal that this build expects is not evidence of a fault, and
	// attaching the CLI's own words to it is how the old message came to
	// quote "password has been deleted." as proof of a failed delete.
	Err error
}

// keychainMissLevel picks how loudly to report a keychain access that
// did not serve the caller.
//
// "No session to open it in" is not a fault: `sudo waired init` reaches
// its user through a hop that leaves the child outside that user's
// bootstrap namespace, and a LaunchDaemon has no session at all, so this
// is the steady state on those paths rather than an event. It used to be
// a WARN with a raw error blob attached, on every read and every write
// (waired-agent#799). Anything else is a real failure and keeps its
// warning.
//
// Read reports it at Debug and Write at Info: the write is the moment
// the choice of store is actually made and is worth one line, while a
// read is a consequence of that choice and repeats for the life of the
// process.
func keychainMissLevel(err error) slog.Level {
	if errors.Is(err, keychain.ErrNoSession) {
		return slog.LevelDebug
	}
	return slog.LevelWarn
}

// clearFailed reports whether the stale-entry sweep left something
// behind. Absent and unsupported are both "nothing there to shadow the
// file"; only a real failure is.
func clearFailed(deleteErr error) bool {
	return deleteErr != nil &&
		!errors.Is(deleteErr, keychain.ErrNotFound) &&
		!errors.Is(deleteErr, keychain.ErrUnsupported)
}

// classifyWrite says what the Keychain half of a Write actually did.
//
// setErr is Set's result; deleteErr is the stale-entry sweep's, or nil
// when no sweep was needed. The sweep exists for the never-stale
// invariant: Read is Keychain-first, so an item left over from an
// earlier write would be preferred over the file that was just updated.
// It is worth attempting even when the keychain refused the write —
// security(1) deletes from a locked keychain quite happily (measured
// 2026-08-21, macOS 26.6.2), so the invariant usually survives the
// refusal.
func classifyWrite(setErr, deleteErr error) writeReport {
	switch {
	case setErr == nil:
		return writeReport{slog.LevelDebug, "securestore: secret written to file and keychain", nil}

	case errors.Is(setErr, keychain.ErrUnsupported):
		// The steady state everywhere but macOS.
		return writeReport{slog.LevelDebug, "securestore: secret written to file (keychain unsupported)", nil}

	case errors.Is(setErr, keychain.ErrNoSession) && !clearFailed(deleteErr):
		// Not a fault of this host. `sudo waired init` reaches its
		// user through a hop that leaves the child outside that
		// user's bootstrap namespace, so securityd has nowhere to
		// put a prompt and refuses; the same call from a desktop
		// session succeeds. The file is authoritative and there is
		// nothing left to shadow it, so there is nothing to warn
		// about — this used to be a WARN on every single init
		// (waired-agent#799).
		return writeReport{slog.LevelInfo,
			"securestore: keychain not reachable from this session; the protected file holds the secret", nil}

	case errors.Is(setErr, keychain.ErrNoSession):
		return writeReport{slog.LevelWarn,
			"securestore: keychain not reachable from this session, and an older entry could not be cleared; it may shadow the file",
			deleteErr}

	case !clearFailed(deleteErr):
		return writeReport{slog.LevelWarn,
			"securestore: keychain write failed; the protected file holds the secret", setErr}

	default:
		return writeReport{slog.LevelWarn,
			"securestore: keychain write failed and an older entry could not be cleared; it may shadow the file",
			errors.Join(setErr, deleteErr)}
	}
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
	setErr := st.Set(item, data)
	var deleteErr error
	if setErr != nil && !errors.Is(setErr, keychain.ErrUnsupported) {
		// Never-stale invariant: a partial/failed Set must not leave an old
		// value that a Keychain-first Read would prefer over the new file.
		deleteErr = st.Delete(item)
	}
	report := classifyWrite(setErr, deleteErr)
	attrs := []any{"service", item.Service}
	if report.Err != nil {
		attrs = append(attrs, "err", report.Err)
	}
	switch report.Level {
	case slog.LevelDebug:
		logger.Debug(report.Msg, attrs...)
	case slog.LevelInfo:
		logger.Info(report.Msg, attrs...)
	default:
		logger.Warn(report.Msg, attrs...)
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
	logger.Debug("securestore: secret removed from file and keychain", "service", item.Service)
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

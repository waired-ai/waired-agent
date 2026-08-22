package integration

import (
	"errors"
	"os"
	"path/filepath"

	"github.com/waired-ai/waired-agent/internal/platform/secrets"
)

// Paths bundles the on-disk locations the integration package owns.
//
// Layout under StateDir:
//
//	integrations/applied.json        (0644, ledger of what was applied)
type Paths struct {
	StateDir       string
	SecretsDir     string
	IntegrationDir string // integrations/
	Ledger         string // integrations/applied.json
}

// PathsFor returns Paths under stateDir, ensuring the directory tree
// exists. secrets/ is locked down via platform/secrets.SecureDir
// (Unix 0700, Windows DACL) because the rest of the agent keeps its
// secrets there; integrations/ stays 0o755, holding only the ledger.
// Write paths (ledger save) use this; read paths use PathsUnder so a
// non-root read of a root-owned state dir surfaces the real EACCES
// instead of a chmod EPERM from SecureDir.
func PathsFor(stateDir string) (*Paths, error) {
	p, err := PathsUnder(stateDir)
	if err != nil {
		return nil, err
	}
	if err := secrets.SecureDir(p.SecretsDir); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(p.IntegrationDir, 0o755); err != nil {
		return nil, err
	}
	return p, nil
}

// PathsUnder returns the Paths layout under stateDir without touching
// the filesystem. Read paths use this so a status query never creates
// or re-permissions directories — a non-root read of a root-owned
// state dir must surface the real EACCES from the read, not a chmod
// EPERM from SecureDir.
func PathsUnder(stateDir string) (*Paths, error) {
	if stateDir == "" {
		return nil, errors.New("integration: empty state dir")
	}
	return &Paths{
		StateDir:       stateDir,
		SecretsDir:     filepath.Join(stateDir, "secrets"),
		IntegrationDir: filepath.Join(stateDir, "integrations"),
		Ledger:         filepath.Join(stateDir, "integrations", "applied.json"),
	}, nil
}

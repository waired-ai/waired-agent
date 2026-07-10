// Package secrets owns the on-disk protection model for the waired
// agent's sensitive state (private keys, access tokens, gateway tokens,
// inference state, preferences).
//
// On Unix systems protection is the conventional POSIX mode bits:
// directories are 0700 and Secret files are 0600. On Windows mode bits
// are meaningless — protection comes from NTFS DACLs applied via
// SetNamedSecurityInfo, granting Full Control to SYSTEM, the local
// Administrators group, and the current user's SID, with inheritance
// disabled.
//
// All writes are atomic (temp file + rename). The temp file is removed
// on any error before the rename, so partial state cannot leak even if
// the caller's process is killed mid-write.
package secrets

// Sensitivity classifies a file's protection requirement.
type Sensitivity int

const (
	// NonSecret files (identity.json, manifests, signed certs from the
	// CP) are world-readable on Unix (0o644). Their content is safe to
	// share — the protection model relies on the on-wire signatures
	// they carry, not on filesystem secrecy.
	NonSecret Sensitivity = iota

	// Secret files (private keys, access tokens) must not be readable
	// by other users. On Unix the file is 0o600 (parent should be 0700
	// — call SecureDir on the parent first). On Windows the file's
	// DACL is replaced with SYSTEM + Administrators + current user
	// only, with inheritance from the parent disabled.
	Secret
)

// WriteFile writes data to path atomically (temp file in the parent
// directory, then rename), applying the protection appropriate to s.
// The parent directory must already exist; use SecureDir for the
// canonical "create + lock down" call.
//
// On any error during write, the temp file is removed before the call
// returns, so callers do not need to worry about partial state.
func WriteFile(path string, data []byte, s Sensitivity) error {
	return writeFile(path, data, s)
}

// WriteSecret is the common-case convenience for WriteFile(path, data, Secret).
func WriteSecret(path string, data []byte) error {
	return writeFile(path, data, Secret)
}

// SecureDir creates path (and any missing parents) and applies a strict
// protection model suitable for holding Secret files: 0o700 on Unix,
// SYSTEM + Administrators + current user DACL with inheritance disabled
// on Windows. Idempotent — calling on an existing directory re-applies
// the protection.
func SecureDir(path string) error {
	return secureDir(path)
}

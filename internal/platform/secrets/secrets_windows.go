//go:build windows

package secrets

import (
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/windows"
)

// SDDL templates: SYSTEM Full Control + BuiltinAdministrators Full +
// the current user Full. Inheritance from the parent is disabled (the
// P flag on the DACL) so a permissive ACL on %ProgramData% cannot
// re-open these files to other users.
//
//	D:P            protected DACL (no inheritance)
//	(A;;FA;;;SY)   ACE: Allow / no inherit / File All / no audit / NT AUTHORITY\SYSTEM
//	(A;;FA;;;BA)   ACE: Allow / no inherit / File All / no audit / BUILTIN\Administrators
//	(A;;FA;;;<sid>) ACE: same for the current user
//
// For directories the OICI inherit flags are added so files newly
// created inside the dir inherit the same DACL.
const (
	daclFileTmpl = "D:P(A;;FA;;;SY)(A;;FA;;;BA)(A;;FA;;;%s)"
	daclDirTmpl  = "D:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)(A;OICI;FA;;;%s)"
)

func writeFile(path string, data []byte, s Sensitivity) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp.*")
	if err != nil {
		return fmt.Errorf("secrets: create temp in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpName)
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("secrets: write %s: %w", tmpName, err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("secrets: fsync %s: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("secrets: close %s: %w", tmpName, err)
	}
	if s == Secret {
		// Apply ACL to the temp BEFORE rename so the rename atomically
		// publishes a file that is already locked down. Renaming
		// preserves the source's security descriptor.
		if err := applyDACL(tmpName, daclFileTmpl); err != nil {
			return fmt.Errorf("secrets: lock %s: %w", tmpName, err)
		}
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("secrets: rename %s -> %s: %w", tmpName, path, err)
	}
	cleanup = false
	return nil
}

func secureDir(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil { // mode is ignored on Windows
		return fmt.Errorf("secrets: mkdir %s: %w", path, err)
	}
	return applyDACL(path, daclDirTmpl)
}

// applyDACL replaces the DACL of the named object with one built from
// the supplied SDDL template (with %s substituted by the current
// process's user SID).
func applyDACL(name, template string) error {
	sidStr, err := currentUserSIDString()
	if err != nil {
		return fmt.Errorf("get current SID: %w", err)
	}
	sddl := fmt.Sprintf(template, sidStr)
	sd, err := windows.SecurityDescriptorFromString(sddl)
	if err != nil {
		return fmt.Errorf("parse SDDL %q: %w", sddl, err)
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return fmt.Errorf("extract DACL: %w", err)
	}
	return windows.SetNamedSecurityInfo(
		name,
		windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil, nil, dacl, nil,
	)
}

func currentUserSIDString() (string, error) {
	token := windows.GetCurrentProcessToken()
	user, err := token.GetTokenUser()
	if err != nil {
		return "", err
	}
	return user.User.Sid.String(), nil
}

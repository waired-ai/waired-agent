# `packaging/windows/` -- Windows GUI installer (Inno Setup 6)

End-user-facing GUI installer for Waired on Windows. Produces a single
self-extracting `WairedSetup-<version>-x64.exe` that double-clicks
into an admin-elevated wizard. The PowerShell one-liner
(`packaging/install/install.ps1`) is the recommended path while
Authenticode signing is not in place -- the GUI is here for users
who would rather click than paste, and for `winget install` once the
manifest lands.

## Layout

```
packaging/windows/
  waired-setup.iss        Inno Setup script (the only authoring file)
  make-zip.ps1            Pack the dist zip + sha256 (called by Makefile)
  README.md               This file.
```

The installer icon is reused from
`internal/gui/tray/icons/waired-connected.ico` to avoid duplicating a
binary asset; the path is referenced via `SetupIconFile=..\..\...` in
the `.iss`. A higher-resolution icon can be dropped at
`packaging/windows/assets/waired.ico` and the .iss switched over
without touching the install flow.

## Building locally

Prereqs: Windows host with Go + Inno Setup 6 installed
(<https://jrsoftware.org/isdl.php>). `iscc.exe` must be on PATH
(`choco install innosetup -y` does this).

```powershell
# From the repo root, in PowerShell or Git Bash:
make dist-windows-installer        # produces dist\windows-amd64\*.exe + zip + sha256
iscc /DAppVersion=1.2.3 packaging\windows\waired-setup.iss

# Output:
ls dist\WairedSetup-1.2.3-x64.exe
```

From Git Bash, escape the `/D` switch as `//D` to bypass MSYS path
conversion:

```sh
iscc //DAppVersion=1.2.3 packaging/windows/waired-setup.iss
```

Omitting `/DAppVersion` falls back to the default `0.0.0-dev`.

## What the installer does

Resolved at install time by the `[Run]` / `[UninstallRun]` sections of
`waired-setup.iss` -- see the comments in the script for the full
ordering.

1. Self-elevates (`PrivilegesRequired=admin`).
2. Extracts `waired.exe`, `waired-agent.exe`, `waired-tray.exe`, and
   the `VERSION` file under `%ProgramFiles%\Waired\`.
3. Runs `waired-agent.exe install`. The Go side does all the
   privileged work (SCM CreateService, recovery actions, Event Log
   source under "Application", restrictive DACL on
   `%ProgramData%\waired\` and `\secrets\`). The script intentionally
   does NOT shell out to `sc.exe` -- there is one source of truth and
   it lives in `internal/platform/service/service_windows.go`.
4. Creates Start Menu entries for the tray and a "Waired (CLI)" help
   shortcut.
5. Offers a Finish-page checkbox to launch `waired-tray.exe`
   immediately. On first launch, the tray writes its own
   `HKCU\Software\Microsoft\Windows\CurrentVersion\Run` entry via
   `internal/platform/autostart/autostart_windows.go`, so subsequent
   logons auto-start it for that user.
6. On uninstall: runs `waired-agent.exe uninstall`, then removes the
   `{app}` directory. `%ProgramData%\waired\` is preserved unless the
   user opts in via the post-uninstall confirmation dialog -- mirrors
   the Debian `apt remove` (preserve) vs `apt purge` (drop) split.

## AppId

`{B4F8A1C2-3D5E-4F6A-9B8C-7D1E2F3A4B5C}`. This is the immutable
identity Inno Setup uses to detect prior versions for in-place upgrade.
**Never change it.** If it changes between releases, Inno treats the
old install as a separate product and you end up with two copies
side-by-side.

## Code signing (deferred)

The Setup.exe is unsigned in the first iteration. SmartScreen will
show "Windows protected your PC" on first run; users click
"More info -> Run anyway". The PowerShell one-liner path
(`install.ps1`) avoids the SmartScreen prompt entirely, which is why
it stays the recommended entry point until signing is in place.

When a code-signing identity is available, drop the PFX (base64) into
`secrets.WAIRED_SIGN_CERT` + the password into
`secrets.WAIRED_SIGN_CERT_PASSWORD` and the `Sign Windows GUI
installer (optional)` step in `.github/workflows/release.yml`
activates. See `docs/decisions.md` for the long-form rationale and
`docs/todo.md` for the unblock condition (agent OSS -> SignPath
Foundation application).

## Verification

`iscc` validates the `.iss` at compile time and surfaces syntax /
referenced-file errors loudly. End-to-end:

```powershell
# Build the artifacts
make dist-windows-installer
iscc /DAppVersion=1.2.3 packaging\windows\waired-setup.iss

# Run inside a clean Windows 11 VM
.\dist\WairedSetup-1.2.3-x64.exe

# Verify after the wizard:
Get-Service waired-agent         # StartType=AutomaticDelayedStart, Status=Running (once enrolled)
& "$env:ProgramFiles\Waired\waired.exe" status
Test-Path "$env:ProgramData\waired\secrets"
icacls.exe "$env:ProgramData\waired\secrets"   # confirm restricted DACL
```

Uninstall via Settings -> Apps -> Waired -> Uninstall. Confirm
service is gone, `%ProgramFiles%\Waired\` is empty, and
`%ProgramData%\waired\` is preserved unless you chose to drop it.

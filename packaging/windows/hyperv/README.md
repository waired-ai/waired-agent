# `packaging/windows/hyperv/` — Windows edge-release VM test harness

Real-machine-equivalent test of the **published Windows edge release**
(`waired-ai/waired-install` :: `edge` prerelease) inside a Hyper-V Windows 11
VM, driven entirely from a **non-interactive Session-0 shell** (Claude Code /
CI). This is the Windows analogue of the Linux LXD installer-test harness and
the macOS tart flow.

## Why Hyper-V (not Windows Sandbox)

Windows Sandbox requires an **interactive desktop session**. Claude Code's
command shell runs in **Session 0 (`UserInteractive=False`)**, so
`WindowsSandbox.exe` launched from it silently exits without creating the
sandbox VM — and a sandbox cannot be controlled/closed from Session 0 either.
This is a Windows session-isolation constraint, not a permissions problem
(the shell runs at High integrity). Hyper-V is managed by the VMMS service and
is fully controllable from Session 0: `New-VM` / `Start-VM` / `Stop-VM` /
`Remove-VM` / `Checkpoint-VM` / `Export-VM`, plus **PowerShell Direct**
(`Invoke-Command -VMName`) to drive the guest with no network or console.

Details: `docs/knowledges/20260613/2201-sandbox-session0-limitation.md`.

## Prerequisites

- Windows 11 Pro host with Hyper-V enabled (`Get-VMHost` works), the
  `Default Switch` (NAT, gives the guest internet), and free disk ≥ ~40 GB.
- A Windows 11 install ISO (retail multi-edition is fine). No baked-in path —
  pass `-IsoPath <win11.iso>` to `New-WairedTestVM.ps1`, or set
  `$env:WAIRED_TEST_ISO`.
- `gcloud` authenticated with permission to impersonate the test login SA
  (`waired-devtest-login@dev-waired.iam.gserviceaccount.com`) for headless
  OIDC enrollment.
- Run from an elevated / High-integrity shell.

## Scripts

| Script | Purpose |
|--------|---------|
| `New-WairedTestVM.ps1`   | Build + start a Gen2 Win11 VM (vTPM + Secure Boot, so no Win11-requirement bypass). Builds an autounattend ISO (IMAPI2, no ADK), installs Win11 Pro unattended, creates `wadmin`/`wuser`, autologons `wadmin`, drops `C:\waired-ready.flag`. |
| `Wait-WairedTestVM.ps1`  | Poll via PowerShell Direct until the install finishes (ready flag). |
| `Invoke-EdgeVmVerify.ps1`| Drive the edge installer/agent/proxy test in the guest; emits `edge-vm-result.json`. |
| `Reset-WairedTestVM.ps1` | **Checkpoint reuse** — revert this VM to the `clean-os` snapshot (seconds). |
| `Export-WairedTestVM.ps1`| **Backup reuse** — export a portable, importable copy of the clean VM. |
| `Import-WairedTestVM.ps1`| Restore a VM from an `Export-WairedTestVM.ps1` backup (other sessions). |
| `Remove-WairedTestVM.ps1`| Destructive teardown (power off, remove VM + checkpoints + VHDX). |
| `autounattend.xml`       | Unattended Windows Setup answer file (edition, accounts, OOBE skip). |

## Reuse: checkpoint vs backup

Building the OS from the ISO takes ~30–40 min. **Do it once**, then reuse the
clean post-OS-setup image. Two complementary mechanisms — keep **both**:

| | `clean-os` **checkpoint** | **Export** backup |
|---|---|---|
| Scope | this VM, this host | portable folder, importable in **other sessions** |
| Revert speed | seconds (`Reset-WairedTestVM.ps1`) | minutes (`Import-WairedTestVM.ps1`) |
| Survives `Remove-VM`? | no (deleted with the VM) | **yes** (independent copy on disk) |
| Use for | fast iteration within a session | new session / after teardown / safety copy |
| Location | inside the VM's checkpoint chain | `C:\waired-vm-backup\waired-edge\` (override: `-BackupRoot` / `$env:WAIRED_VM_BACKUP`) |

Both are captured **once, right after OS setup and before any waired bits are
installed**, so they are a bare clean Windows baseline.

## Quick start (first run — builds the OS once)

```powershell
# point $h at this harness folder in your checkout, and give it an ISO:
$h = "$PWD"   # if your shell is already in packaging\windows\hyperv (else an absolute path)
$env:WAIRED_TEST_ISO = 'D:\isos\Win11.iso'   # or pass -IsoPath to New-WairedTestVM.ps1

# 1. Build + start the VM (unattended Win11 install, ~30-40 min)
& "$h\New-WairedTestVM.ps1"            # -IsoPath ... to override the ISO

# 2. Wait until the guest is ready
& "$h\Wait-WairedTestVM.ps1"

# 3. Capture the reusable baselines (checkpoint + portable backup)
& "$h\Export-WairedTestVM.ps1"         # also creates the 'clean-os' checkpoint
Start-VM -Name waired-edge             # Export powers off; bring it back up

# 4. Run the edge installer/agent/proxy verification
& "$h\Invoke-EdgeVmVerify.ps1"         # writes ...\out\edge-vm-result.json
```

## Reuse workflows

**Fast iteration in the same session** (re-run the test on a clean OS):

```powershell
& "$h\Invoke-EdgeVmVerify.ps1" -Fresh  # reverts to 'clean-os' first, then tests
# or manually:
& "$h\Reset-WairedTestVM.ps1" -Start
```

**New session, or after a full teardown** (restore from the portable backup):

```powershell
& "$h\Import-WairedTestVM.ps1" -BackupRoot C:\waired-vm-backup
Start-VM -Name waired-edge
& "$h\Reset-WairedTestVM.ps1"          # (re)create the 'clean-os' checkpoint
& "$h\Invoke-EdgeVmVerify.ps1"
```

**Done for good** (free disk; the export backup is kept):

```powershell
& "$h\Remove-WairedTestVM.ps1"         # add -KeepVhd to retain the live VHDX
```

## Notes / caveats

- **Lab credentials**: `wadmin` / `wuser` both use `Waired!Test123` on a
  disposable VM. Not secrets.
- **vTPM + Export (already-set-up host only)**: the Export/Import (and
  checkpoint) reuse path assumes a host that is **already set up** — the
  exported key protector is bound to that host's Hyper-V guardian, so Import
  works only on the **same host**. On a **fresh / different host**, don't
  Import; set up from scratch by building from the ISO with
  `New-WairedTestVM.ps1` (cross-host import would also need the guardian
  exported, which is out of scope).
- **RAM during inference**: the bundled model (qwen2.5-coder-7b, q4
  ~4.7 GB weights) needs the VM to actually *have* the RAM when the agent's
  picker runs. `New-WairedTestVM.ps1` sets a **12 GB dynamic-memory floor**
  (`-MemoryMinGB`) for exactly this reason: with the old 2 GB floor Hyper-V
  reclaimed the idle VM down to ~3 GB, the picker under-sized the model, and
  the gateway returned `422 hardware_insufficient`. If the *host* is tight,
  `wsl --shutdown` frees the `vmmemWSL` allocation before the inference phase.
- **UAC blind spot**: `Invoke-EdgeVmVerify.ps1` runs elevated via PowerShell
  Direct, so install.ps1's standard-user → `Start-Process -Verb RunAs`
  *consent click* is not exercised (same gap Windows Sandbox has). The
  post-elevation install logic is fully covered. To cover the consent, open
  `vmconnect` as `wuser` and run the one-liner once (interactive).
- **Headless enrollment**: `Invoke-EdgeVmVerify.ps1` mints a Google OIDC
  id_token on the host (`gcloud ... print-identity-token --impersonate-service-account`)
  and passes it to `waired init --google-sa-login --oidc-id-token` — no
  browser OAuth needed.

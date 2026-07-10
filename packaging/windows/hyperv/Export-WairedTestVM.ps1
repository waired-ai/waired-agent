#Requires -Version 5.1
<#
.SYNOPSIS
    Export the post-OS-setup Waired test VM as a portable, importable backup so
    other sessions can reuse the clean Windows 11 image without re-installing.

.DESCRIPTION
    Two complementary reuse mechanisms:
      * 'clean-os' CHECKPOINT (Reset-WairedTestVM.ps1)  -> fast in-place revert
        on THIS VM (seconds).
      * EXPORT (this script)                            -> self-contained backup
        (VM config + VHDX + vTPM key protector) under a stable folder, restorable
        in any other session via Import-WairedTestVM.ps1 (same host).

    Recommended once, right after the unattended install reaches the desktop and
    before any waired bits are installed, so the backup is a bare clean OS.

    A graceful shutdown is done first (offline export = smallest, most consistent
    baseline). vTPM note: the exported key protector is encrypted to this host's
    guardian, so import works on the SAME host. Cross-host reuse would require
    exporting/importing the guardian too (out of scope).
#>
[CmdletBinding()]
param(
    [string]$VmName = 'waired-edge',
    [string]$BackupRoot = $(if ($env:WAIRED_VM_BACKUP) { $env:WAIRED_VM_BACKUP } else { 'C:\waired-vm-backup' }),
    [string]$CheckpointName = 'clean-os',
    [switch]$NoShutdown,
    [string]$GuestAdmin = 'wadmin',
    [string]$GuestPassword = 'Waired!Test123'
)
$ErrorActionPreference = 'Stop'
function Log { param([string]$m) Write-Host "[export] $m" -ForegroundColor Cyan }

$vm = Get-VM -Name $VmName -ErrorAction SilentlyContinue
if (-not $vm) { Write-Host "[export] VM '$VmName' not found" -ForegroundColor Red; exit 1 }

# 1. Ensure the fast in-place checkpoint exists too.
if (-not (Get-VMCheckpoint -VMName $VmName -Name $CheckpointName -ErrorAction SilentlyContinue)) {
    Log "creating '$CheckpointName' checkpoint (in-place fast revert)"
    Checkpoint-VM -Name $VmName -SnapshotName $CheckpointName
}

# 2. Graceful shutdown for a clean offline export.
if (-not $NoShutdown -and $vm.State -eq 'Running') {
    Log "graceful shutdown for clean export"
    try {
        $sec  = ConvertTo-SecureString $GuestPassword -AsPlainText -Force
        $cred = New-Object System.Management.Automation.PSCredential("$VmName\$GuestAdmin", $sec)
        Invoke-Command -VMName $VmName -Credential $cred -ScriptBlock { Stop-Computer -Force } -ErrorAction Stop
    } catch {
        Log "PS Direct shutdown failed ($($_.Exception.Message)); using Stop-VM"
        Stop-VM -Name $VmName -Force
    }
    $deadline = (Get-Date).AddMinutes(5)
    while ((Get-VM -Name $VmName).State -ne 'Off' -and (Get-Date) -lt $deadline) { Start-Sleep -Seconds 3 }
    if ((Get-VM -Name $VmName).State -ne 'Off') { Stop-VM -Name $VmName -TurnOff -Force }
    Log "VM powered off"
}

# 3. Export.
$dest = Join-Path $BackupRoot $VmName
if (Test-Path -LiteralPath $dest) {
    Log "removing stale export at $dest"
    [System.IO.Directory]::Delete($dest, $true)
}
New-Item -ItemType Directory -Path $BackupRoot -Force | Out-Null
Log "exporting '$VmName' -> $BackupRoot (this can take a few minutes)"
Export-VM -Name $VmName -Path $BackupRoot
$sizeGB = [math]::Round(((Get-ChildItem -LiteralPath $dest -Recurse -File | Measure-Object Length -Sum).Sum/1GB),2)
Log "export complete: $dest (${sizeGB}GB)"
Log "restore in another session: Import-WairedTestVM.ps1 -BackupRoot $BackupRoot"

# 4. Optionally power back on to continue testing in this session.
Log "(VM left powered off; Start-VM -Name $VmName to resume)"

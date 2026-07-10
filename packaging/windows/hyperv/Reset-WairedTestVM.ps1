#Requires -Version 5.1
<#
.SYNOPSIS
    Revert the Waired edge test VM to its reusable 'clean-os' checkpoint so a
    fresh installer run starts from a bare, post-OS-setup Windows 11 — no
    multi-GB ISO re-install required.

.DESCRIPTION
    The 'clean-os' checkpoint is taken once, right after the unattended Win11
    install finishes (before any waired bits are installed). Reverting to it is
    a few seconds vs ~30-40 min to reinstall. Use this between test iterations.

    First run (no checkpoint yet) creates it instead of reverting, so the script
    is idempotent and self-bootstrapping when paired with a freshly-installed VM.
#>
[CmdletBinding()]
param(
    [string]$VmName = 'waired-edge',
    [string]$CheckpointName = 'clean-os',
    [switch]$Start
)
$ErrorActionPreference = 'Stop'
function Log { param([string]$m) Write-Host "[reset] $m" -ForegroundColor Cyan }

$vm = Get-VM -Name $VmName -ErrorAction SilentlyContinue
if (-not $vm) { Write-Host "[reset] VM '$VmName' not found" -ForegroundColor Red; exit 1 }

$cp = Get-VMCheckpoint -VMName $VmName -Name $CheckpointName -ErrorAction SilentlyContinue
if (-not $cp) {
    Log "no '$CheckpointName' checkpoint yet -> creating it from current state"
    Checkpoint-VM -Name $VmName -SnapshotName $CheckpointName
    Log "created checkpoint '$CheckpointName'"
    exit 0
}

Log "reverting '$VmName' to checkpoint '$CheckpointName'"
if ($vm.State -ne 'Off') { Stop-VM -Name $VmName -TurnOff -Force }
Restore-VMCheckpoint -VMName $VmName -Name $CheckpointName -Confirm:$false
Log "reverted."
if ($Start) { Start-VM -Name $VmName; Log "started." }

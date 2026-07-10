#Requires -Version 5.1
<#
.SYNOPSIS
    Fully tear down the Waired edge test VM: power off, remove the VM and its
    checkpoints, and delete the VHDX. Use only when you are done with the VM.

.DESCRIPTION
    For routine iteration prefer Reset-WairedTestVM.ps1 (revert to 'clean-os'),
    which keeps the reusable OS image. This script is the destructive teardown.
    The Win11 install ISO supplied to New-WairedTestVM.ps1 is never touched.
#>
[CmdletBinding()]
param(
    [string]$VmName = 'waired-edge',
    [switch]$KeepVhd
)
$ErrorActionPreference = 'Stop'
function Log { param([string]$m) Write-Host "[remove] $m" -ForegroundColor Cyan }

$vm = Get-VM -Name $VmName -ErrorAction SilentlyContinue
if (-not $vm) { Log "VM '$VmName' not found (already gone)"; exit 0 }

if ($vm.State -ne 'Off') { Log "powering off"; Stop-VM -Name $VmName -TurnOff -Force }

$vhds = @($vm.HardDrives | ForEach-Object { $_.Path })
Log "removing checkpoints + VM definition"
Get-VMCheckpoint -VMName $VmName -ErrorAction SilentlyContinue | Remove-VMCheckpoint -Confirm:$false -ErrorAction SilentlyContinue
Remove-VM -Name $VmName -Force

if (-not $KeepVhd) {
    foreach ($v in $vhds) {
        if ($v -and (Test-Path -LiteralPath $v)) {
            try { [System.IO.File]::Delete($v); Log "deleted VHDX $v" } catch { Log "could not delete $v : $($_.Exception.Message)" }
        }
    }
} else { Log "kept VHDX(s): $($vhds -join ', ')" }
Log "teardown complete."

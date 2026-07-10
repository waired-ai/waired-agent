#Requires -Version 5.1
<#
.SYNOPSIS
    Restore the Waired test VM from an Export-WairedTestVM.ps1 backup so a new
    session gets the clean Windows 11 image without re-installing (~minutes vs
    ~30-40 min for a fresh unattended install).

.DESCRIPTION
    Imports with -Copy + -GenerateNewId so the on-disk backup stays pristine and
    can be imported repeatedly. Same-host reuse (vTPM key protector is bound to
    this host's guardian). Reconnects the NIC to the Default Switch.
#>
[CmdletBinding()]
param(
    [string]$BackupRoot = $(if ($env:WAIRED_VM_BACKUP) { $env:WAIRED_VM_BACKUP } else { 'C:\waired-vm-backup' }),
    [string]$SourceVmName = 'waired-edge',
    [string]$NewVmName = 'waired-edge',
    [string]$SwitchName = 'Default Switch',
    [string]$VhdDestination = (Join-Path (Get-VMHost).VirtualHardDiskPath 'waired-edge-imported'),
    [string]$VmDestination  = (Join-Path (Get-VMHost).VirtualMachinePath  'waired-edge-imported')
)
$ErrorActionPreference = 'Stop'
function Log { param([string]$m) Write-Host "[import] $m" -ForegroundColor Cyan }

$srcDir = Join-Path $BackupRoot $SourceVmName
$vmcx = Get-ChildItem -LiteralPath (Join-Path $srcDir 'Virtual Machines') -Filter *.vmcx -ErrorAction SilentlyContinue | Select-Object -First 1
if (-not $vmcx) { Write-Host "[import] no .vmcx under $srcDir\Virtual Machines" -ForegroundColor Red; exit 1 }
Log "source config: $($vmcx.FullName)"

if (Get-VM -Name $NewVmName -ErrorAction SilentlyContinue) {
    Write-Host "[import] a VM named '$NewVmName' already exists; remove it first (Remove-WairedTestVM.ps1)" -ForegroundColor Red
    exit 1
}

New-Item -ItemType Directory -Path $VhdDestination -Force | Out-Null
New-Item -ItemType Directory -Path $VmDestination  -Force | Out-Null

Log "importing (Copy + new Id) ..."
$vm = Import-VM -Path $vmcx.FullName -Copy -GenerateNewId `
        -VhdDestinationPath $VhdDestination -VirtualMachinePath $VmDestination
if ($vm.Name -ne $NewVmName) { Rename-VM -VM $vm -NewName $NewVmName }
Connect-VMNetworkAdapter -VMName $NewVmName -SwitchName $SwitchName -ErrorAction SilentlyContinue
Log "imported as '$NewVmName'. Start-VM -Name $NewVmName to use it."

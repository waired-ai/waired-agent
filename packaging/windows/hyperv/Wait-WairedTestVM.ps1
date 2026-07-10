#Requires -Version 5.1
<#
.SYNOPSIS
    Poll a Hyper-V VM via PowerShell Direct until the unattended Win11 install
    finishes (C:\waired-ready.flag dropped by autounattend FirstLogonCommands).

.DESCRIPTION
    No network or interactive console needed: PowerShell Direct authenticates
    against the guest's local wadmin account over VMBus. During WinPE/OOBE the
    connection is refused; we swallow that and keep polling.
#>
[CmdletBinding()]
param(
    [string]$VmName = 'waired-edge',
    [string]$GuestUser = 'wadmin',
    [string]$GuestPassword = 'Waired!Test123',
    [int]$TimeoutMinutes = 60,
    [int]$IntervalSeconds = 30
)
$ErrorActionPreference = 'Stop'
function Log { param([string]$m) Write-Host ("[wait {0}] {1}" -f (Get-Date -Format 'HH:mm:ss'), $m) }

$sec = ConvertTo-SecureString $GuestPassword -AsPlainText -Force
$cred = New-Object System.Management.Automation.PSCredential("$VmName\$GuestUser", $sec)

$deadline = (Get-Date).AddMinutes($TimeoutMinutes)
$ready = $false
while ((Get-Date) -lt $deadline) {
    $vm = Get-VM -Name $VmName -ErrorAction SilentlyContinue
    if (-not $vm) { Log "VM '$VmName' not found"; exit 2 }
    $hb = (Get-VMIntegrationService -VMName $VmName -Name 'Heartbeat' -ErrorAction SilentlyContinue).PrimaryStatusDescription
    try {
        $flag = Invoke-Command -VMName $VmName -Credential $cred -ErrorAction Stop -ScriptBlock {
            Test-Path 'C:\waired-ready.flag'
        }
        if ($flag) {
            $info = Invoke-Command -VMName $VmName -Credential $cred -ScriptBlock {
                [pscustomobject]@{
                    Host  = $env:COMPUTERNAME
                    Build = (Get-ItemProperty 'HKLM:\SOFTWARE\Microsoft\Windows NT\CurrentVersion').DisplayVersion
                    User  = $env:USERNAME
                }
            }
            Log ("READY: guest='{0}' build={1} (PS Direct as {2})" -f $info.Host, $info.Build, $info.User)
            $ready = $true
            break
        } else {
            Log "PS Direct up but ready-flag absent yet (state=$($vm.State), heartbeat=$hb)"
        }
    } catch {
        Log "installing... (state=$($vm.State), heartbeat=$hb)"
    }
    Start-Sleep -Seconds $IntervalSeconds
}

if (-not $ready) { Log "TIMEOUT after $TimeoutMinutes min"; exit 1 }
Log "VM is ready for the edge install test."
exit 0

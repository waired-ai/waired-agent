#Requires -Version 5.1
<#
.SYNOPSIS
    Create a clean Windows 11 Hyper-V VM for the Waired edge-release test,
    fully headless from Session 0 — NO DVD boot, NO "press any key" prompt.

.DESCRIPTION
    Windows Sandbox cannot run from Claude's Session-0 / non-interactive shell
    (docs/knowledges/20260613/2201-sandbox-session0-limitation.md), so the test
    runs in a Hyper-V VM driven via PowerShell Direct.

    A Gen2 VM booting a Windows ISO stops at the firmware "Press any key to boot
    from CD or DVD" prompt, which cannot be answered from Session 0 (no console
    keypress) — the install never starts (VHDX stays 0 bytes, CPU 0%). To stay
    fully headless we therefore do NOT boot the install DVD. Instead we:

      1. Create + partition a VHDX (GPT: EFI + MSR + Windows).
      2. Expand-WindowsImage install.wim (index 6 = Win11 Pro) onto it.
      3. bcdboot to make it UEFI-bootable.
      4. Drop autounattend.xml at \Windows\Panther\unattend.xml so the
         specialize + oobeSystem passes run on FIRST BOOT (create wadmin/wuser,
         skip OOBE, autologon wadmin, drop C:\waired-ready.flag).
      5. Create a Gen2 VM (Secure Boot + vTPM => satisfies Win11 checks) that
         boots straight from the HDD.

    Poll readiness from the host with Wait-WairedTestVM.ps1 (PowerShell Direct).

.PARAMETER IsoPath
    Path to the Windows 11 install ISO (source of install.wim). No baked-in
    default — pass -IsoPath, or set $env:WAIRED_TEST_ISO, so the harness stays
    host-portable.
#>
[CmdletBinding()]
param(
    [string]$IsoPath = $env:WAIRED_TEST_ISO,
    [string]$VmName  = 'waired-edge',
    [int]$WimIndex = 6,                # 6 = Windows 11 Pro in the 25H2 retail wim
    [string]$UnattendXml = (Join-Path $PSScriptRoot 'autounattend.xml'),
    [int]$VhdSizeGB = 64,
    # Dynamic-memory FLOOR. Must be high enough that the agent's model
    # picker sees enough RAM for the bundled qwen2.5-coder-7b (q4 ~4.7 GB
    # weights + runtime overhead). With the old 2 GB floor, Hyper-V
    # reclaimed the idle VM down to ~3 GB, so the picker under-sized the
    # model and the gateway returned 422 (hardware_insufficient) on what
    # is otherwise a 16 GB-max VM. Keep Min ≈ Startup so inference is
    # reliably testable regardless of idle reclaim.
    [int]$MemoryMinGB = 12,
    [int]$MemoryStartupGB = 12,
    [int]$MemoryMaxGB = 16,
    [int]$CpuCount = 6,
    [string]$SwitchName = 'Default Switch'
)

$ErrorActionPreference = 'Stop'
function Log { param([string]$m) Write-Host "[new-vm] $m" -ForegroundColor Cyan }
function Die { param([string]$m) Write-Host "[new-vm] $m" -ForegroundColor Red; exit 1 }

if (-not $IsoPath)                              { Die 'no ISO given. Pass -IsoPath <win11.iso> or set $env:WAIRED_TEST_ISO.' }
if (-not (Test-Path -LiteralPath $IsoPath))     { Die "ISO not found: $IsoPath" }
if (-not (Test-Path -LiteralPath $UnattendXml)) { Die "autounattend.xml not found: $UnattendXml" }

# --- 1. Remove any prior VM of the same name --------------------------------
$existing = Get-VM -Name $VmName -ErrorAction SilentlyContinue
if ($existing) {
    Log "removing pre-existing VM '$VmName'"
    if ($existing.State -ne 'Off') { Stop-VM -Name $VmName -TurnOff -Force }
    Get-VMCheckpoint -VMName $VmName -ErrorAction SilentlyContinue | Remove-VMCheckpoint -Confirm:$false -ErrorAction SilentlyContinue
    $existing.HardDrives | ForEach-Object { if ($_.Path -and (Test-Path -LiteralPath $_.Path)) { [System.IO.File]::Delete($_.Path) } }
    Remove-VM -Name $VmName -Force
}

$vhdDir  = (Get-VMHost).VirtualHardDiskPath
$vhdPath = Join-Path $vhdDir "$VmName.vhdx"
if (Test-Path -LiteralPath $vhdPath) { [System.IO.File]::Delete($vhdPath) }

$isoMounted = $false; $vhdMounted = $false
try {
    # --- 2. Mount ISO, locate install.wim -----------------------------------
    Log "mounting ISO"
    $img = Mount-DiskImage -ImagePath $IsoPath -PassThru
    $isoMounted = $true
    $isoDl = ($img | Get-Volume).DriveLetter
    $wim = "${isoDl}:\sources\install.wim"
    if (-not (Test-Path $wim)) { $wim = "${isoDl}:\sources\install.esd" }
    if (-not (Test-Path $wim)) { Die "no install.wim/.esd in ISO" }
    Log "image: $wim (index $WimIndex)"

    # --- 3. Create + partition the VHDX (GPT: EFI + MSR + Windows) -----------
    Log "creating VHDX $vhdPath (${VhdSizeGB}GB dynamic)"
    $vhd = New-VHD -Path $vhdPath -SizeBytes ($VhdSizeGB * 1GB) -Dynamic
    $disk = Mount-VHD -Path $vhdPath -Passthru | Get-Disk
    $vhdMounted = $true
    Initialize-Disk -Number $disk.Number -PartitionStyle GPT -Confirm:$false | Out-Null

    $efi = New-Partition -DiskNumber $disk.Number -Size 300MB -GptType '{c12a7328-f81f-11d2-ba4b-00a0c93ec93b}'
    $efi | Format-Volume -FileSystem FAT32 -NewFileSystemLabel 'System' -Confirm:$false | Out-Null
    $efi | Add-PartitionAccessPath -AssignDriveLetter
    $efiDl = (Get-Partition -DiskNumber $disk.Number -PartitionNumber $efi.PartitionNumber).DriveLetter

    New-Partition -DiskNumber $disk.Number -Size 16MB -GptType '{e3c9e316-0b5c-4db8-817d-f92df00215ae}' | Out-Null  # MSR

    $win = New-Partition -DiskNumber $disk.Number -UseMaximumSize -GptType '{ebd0a0a2-b9e5-4433-87c0-68b6b72699c7}'
    $win | Format-Volume -FileSystem NTFS -NewFileSystemLabel 'Windows' -Confirm:$false | Out-Null
    $win | Add-PartitionAccessPath -AssignDriveLetter
    $winDl = (Get-Partition -DiskNumber $disk.Number -PartitionNumber $win.PartitionNumber).DriveLetter
    Log "partitions: EFI=${efiDl}: Windows=${winDl}:"

    # --- 4. Apply the image -------------------------------------------------
    Log "applying install image (this takes ~10-20 min)..."
    Expand-WindowsImage -ImagePath $wim -Index $WimIndex -ApplyPath "${winDl}:\" | Out-Null

    # --- 5. Make UEFI-bootable ----------------------------------------------
    Log "bcdboot ${winDl}:\Windows -> EFI ${efiDl}:"
    & "$env:SystemRoot\System32\bcdboot.exe" "${winDl}:\Windows" /s "${efiDl}:" /f UEFI | Out-Null
    if ($LASTEXITCODE -ne 0) { Die "bcdboot failed ($LASTEXITCODE)" }

    # --- 6. Seed unattend for specialize/oobeSystem on first boot -----------
    $panther = "${winDl}:\Windows\Panther"
    New-Item -ItemType Directory -Path $panther -Force | Out-Null
    Copy-Item -LiteralPath $UnattendXml -Destination (Join-Path $panther 'unattend.xml') -Force
    Log "seeded $panther\unattend.xml"
}
finally {
    if ($vhdMounted) { Dismount-VHD -Path $vhdPath -ErrorAction SilentlyContinue }
    if ($isoMounted) { Dismount-DiskImage -ImagePath $IsoPath -ErrorAction SilentlyContinue | Out-Null }
}

# --- 7. Create the Gen2 VM (boot from HDD, no DVD) --------------------------
Log "creating Gen2 VM '$VmName'"
New-VM -Name $VmName -Generation 2 -MemoryStartupBytes ($MemoryStartupGB * 1GB) `
    -VHDPath $vhdPath -SwitchName $SwitchName | Out-Null
Set-VMMemory -VMName $VmName -DynamicMemoryEnabled $true `
    -MinimumBytes ($MemoryMinGB * 1GB) -StartupBytes ($MemoryStartupGB * 1GB) -MaximumBytes ($MemoryMaxGB * 1GB)
Set-VMProcessor -VMName $VmName -Count $CpuCount
Set-VM -Name $VmName -CheckpointType Standard -AutomaticCheckpointsEnabled $false
Set-VMFirmware -VMName $VmName -EnableSecureBoot On -SecureBootTemplate 'MicrosoftWindows'
Set-VMKeyProtector -VMName $VmName -NewLocalKeyProtector
Enable-VMTPM -VMName $VmName
$hdd = Get-VMHardDiskDrive -VMName $VmName
Set-VMFirmware -VMName $VmName -FirstBootDevice $hdd
Log "Secure Boot + vTPM enabled; first boot = HDD"

Start-VM -Name $VmName
Log "VM '$VmName' started. First boot runs specialize+OOBE unattended (~5-12 min)."
Log "Poll readiness: powershell -File Wait-WairedTestVM.ps1 -VmName $VmName"

# uac-probe.ps1 -- can a hosted runner produce a FILTERED administrator token,
# in a session with a desktop, without a VM and without a reboot?
#
# waired-agent#997 says a granted UAC elevation is not automatable here. Two
# routes were measured not to work: a scheduled task (batch logon -> FULL
# token, no window station) and `runas /trustlevel:0x20000` (SAFER-restricted,
# cannot even run Get-FileHash). This probe tests a third: the job already runs
# as an administrator in a session that HAS a window station, so if its token
# is a split token, the LINKED token is exactly the filtered administrator the
# hand-off needs -- no new user, no logon, no reboot.
#
# Reports, never asserts. Every step is independent; a failure in one is data
# for the next. Everything is deadline-bounded: an unsuppressed UAC dialog once
# pinned a run for 28 minutes.
[CmdletBinding()]
param(
    [int]$StepTimeoutSec = 45,
    # Stop after P1. On a DEVELOPER box the linked token of an unelevated admin
    # is the FULL one, so P2 would be a genuine silent elevation of the user's
    # own machine. Local runs are for checking the interop compiles and what
    # the token actually says -- nothing more.
    [switch]$NoLaunch
)

$ErrorActionPreference = 'Continue'
$ProgressPreference    = 'SilentlyContinue'

function Say  { param([string]$m) Write-Host "[uac-probe] $m" }
function Head { param([string]$m) Write-Host "[uac-probe] ==> $m" -ForegroundColor Green }

$Work = Join-Path ([System.IO.Path]::GetTempPath()) "uac-probe-$PID"
New-Item -ItemType Directory -Path $Work -Force | Out-Null

# --------------------------------------------------------------------------
Head 'P0  identity, session, desktop'
# --------------------------------------------------------------------------
$id = [Security.Principal.WindowsIdentity]::GetCurrent()
$pr = New-Object Security.Principal.WindowsPrincipal($id)
Say "  whoami            = $($id.Name)"
Say "  IsAdmin           = $($pr.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator))"
Say "  process sessionId = $((Get-Process -Id $PID).SessionId)"
try   { Say "  console user      = $((Get-CimInstance Win32_ComputerSystem).UserName)" }
catch { Say "  console user      = (unavailable: $($_.Exception.Message))" }
# The web says "UAC is disabled during image setup" on hosted runners; the
# -Contract leg measured EnableLUA=1. Both cannot be right, and it decides
# whether a split token can exist at all -- so read it here rather than cite it.
$uacKey = 'HKLM:\SOFTWARE\Microsoft\Windows\CurrentVersion\Policies\System'
foreach ($n in 'EnableLUA','ConsentPromptBehaviorAdmin','ConsentPromptBehaviorUser','FilterAdministratorToken','PromptOnSecureDesktop','LocalAccountTokenFilterPolicy') {
    $v = try { (Get-ItemProperty -LiteralPath $uacKey -Name $n -ErrorAction Stop).$n } catch { $null }
    Say ("  {0,-28} = {1}" -f $n, $(if ($null -eq $v) { '(absent)' } else { $v }))
}
# RID 500 (the built-in Administrator) is NOT filtered when
# FilterAdministratorToken is 0/absent -- which would mean no split token and
# no linked token, for a reason that has nothing to do with EnableLUA.
Say ("  current user SID             = {0}" -f $id.User.Value)
Say ("  is RID 500 (built-in Admin)  = {0}" -f $id.User.Value.EndsWith('-500'))

$expl = @(Get-Process -Name explorer -ErrorAction SilentlyContinue)
if ($expl.Count) {
    foreach ($e in $expl) {
        $owner = try {
            $o = Invoke-CimMethod -InputObject (Get-CimInstance Win32_Process -Filter "ProcessId=$($e.Id)") -MethodName GetOwner
            "$($o.Domain)\$($o.User)"
        } catch { '(owner unavailable)' }
        Say "  explorer.exe      = pid $($e.Id) session $($e.SessionId) owner $owner"
    }
} else {
    Say '  explorer.exe      = NOT RUNNING'
}

# --------------------------------------------------------------------------
Head 'P1  token elevation type and the linked token'
# --------------------------------------------------------------------------
$cs = @'
using System;
using System.Runtime.InteropServices;

public static class UacProbe {
    public const uint TOKEN_QUERY = 0x0008, TOKEN_DUPLICATE = 0x0002, TOKEN_ASSIGN_PRIMARY = 0x0001;
    public const uint MAXIMUM_ALLOWED = 0x02000000;
    public const int TokenElevationType = 18, TokenLinkedToken = 19, TokenElevation = 20;

    [StructLayout(LayoutKind.Sequential)]
    public struct STARTUPINFO {
        public int cb; public string lpReserved; public string lpDesktop; public string lpTitle;
        public int dwX, dwY, dwXSize, dwYSize, dwXCountChars, dwYCountChars, dwFillAttribute, dwFlags;
        public short wShowWindow, cbReserved2; public IntPtr lpReserved2, hStdInput, hStdOutput, hStdError;
    }
    [StructLayout(LayoutKind.Sequential)]
    public struct PROCESS_INFORMATION { public IntPtr hProcess, hThread; public int dwProcessId, dwThreadId; }

    [DllImport("kernel32.dll")] public static extern IntPtr GetCurrentProcess();
    [DllImport("advapi32.dll", SetLastError=true)]
    public static extern bool OpenProcessToken(IntPtr h, uint access, out IntPtr token);
    [DllImport("advapi32.dll", SetLastError=true)]
    public static extern bool GetTokenInformation(IntPtr token, int cls, IntPtr buf, int len, out int need);
    [DllImport("advapi32.dll", SetLastError=true)]
    public static extern bool DuplicateTokenEx(IntPtr token, uint access, IntPtr attr, int impLevel, int tokenType, out IntPtr dup);
    [DllImport("advapi32.dll", SetLastError=true, CharSet=CharSet.Unicode)]
    public static extern bool CreateProcessWithTokenW(IntPtr token, uint logonFlags, string app, string cmd,
        uint flags, IntPtr env, string dir, ref STARTUPINFO si, out PROCESS_INFORMATION pi);
    [DllImport("kernel32.dll", SetLastError=true)] public static extern bool CloseHandle(IntPtr h);

    // 0 = could not open the token, else the TOKEN_ELEVATION_TYPE (1 default, 2 full, 3 limited)
    public static int ElevationType(out int lastError) {
        lastError = 0;
        IntPtr tok;
        if (!OpenProcessToken(GetCurrentProcess(), TOKEN_QUERY, out tok)) { lastError = Marshal.GetLastWin32Error(); return 0; }
        try {
            IntPtr buf = Marshal.AllocHGlobal(4);
            try {
                int need;
                if (!GetTokenInformation(tok, TokenElevationType, buf, 4, out need)) { lastError = Marshal.GetLastWin32Error(); return 0; }
                return Marshal.ReadInt32(buf);
            } finally { Marshal.FreeHGlobal(buf); }
        } finally { CloseHandle(tok); }
    }

    // The linked token handle, or IntPtr.Zero with lastError set.
    public static IntPtr LinkedToken(out int lastError) {
        lastError = 0;
        IntPtr tok;
        if (!OpenProcessToken(GetCurrentProcess(), TOKEN_QUERY | TOKEN_DUPLICATE, out tok)) { lastError = Marshal.GetLastWin32Error(); return IntPtr.Zero; }
        try {
            IntPtr buf = Marshal.AllocHGlobal(IntPtr.Size);
            try {
                int need;
                if (!GetTokenInformation(tok, TokenLinkedToken, buf, IntPtr.Size, out need)) { lastError = Marshal.GetLastWin32Error(); return IntPtr.Zero; }
                return Marshal.ReadIntPtr(buf);
            } finally { Marshal.FreeHGlobal(buf); }
        } finally { CloseHandle(tok); }
    }

    // Launch cmdLine with `token`, on winsta0\default. Returns the pid, or -1.
    public static int LaunchWith(IntPtr token, string cmdLine, string dir, out int lastError) {
        lastError = 0;
        IntPtr primary;
        // 1 = SecurityIdentification is not enough for a primary token; 2 = SecurityImpersonation, 1 = TokenPrimary
        if (!DuplicateTokenEx(token, MAXIMUM_ALLOWED, IntPtr.Zero, 2, 1, out primary)) { lastError = Marshal.GetLastWin32Error(); return -1; }
        try {
            STARTUPINFO si = new STARTUPINFO();
            si.cb = Marshal.SizeOf(si);
            si.lpDesktop = "winsta0\\default";
            PROCESS_INFORMATION pi;
            if (!CreateProcessWithTokenW(primary, 0, null, cmdLine, 0, IntPtr.Zero, dir, ref si, out pi)) {
                lastError = Marshal.GetLastWin32Error(); return -1;
            }
            CloseHandle(pi.hThread);
            CloseHandle(pi.hProcess);
            return pi.dwProcessId;
        } finally { CloseHandle(primary); }
    }
}
'@

$typeOk = $false
try { Add-Type -TypeDefinition $cs -Language CSharp -ErrorAction Stop; $typeOk = $true; Say '  interop compiled' }
catch { Say "  interop FAILED to compile: $($_.Exception.Message)" }

$elevType = 0
$linked   = [IntPtr]::Zero
if ($typeOk) {
    $err = 0
    $elevType = [UacProbe]::ElevationType([ref]$err)
    $names = @{ 0 = 'ERROR'; 1 = 'Default (NO split token)'; 2 = 'Full (elevated; a linked LIMITED token should exist)'; 3 = 'Limited (filtered)' }
    Say "  TokenElevationType = $elevType  $($names[$elevType])  (lastError=$err)"

    $err = 0
    $linked = [UacProbe]::LinkedToken([ref]$err)
    if ($linked -eq [IntPtr]::Zero) {
        Say "  TokenLinkedToken   = NONE (lastError=$err)  <-- route 3 is dead if this is the case"
    } else {
        Say "  TokenLinkedToken   = present (handle 0x$($linked.ToString('x')))"
    }
}

# --------------------------------------------------------------------------
Head 'P2  a child launched with the linked token: is it a FILTERED admin?'
# --------------------------------------------------------------------------
$childOut = Join-Path $Work 'child.txt'
$childPs  = Join-Path $Work 'child.ps1'
@'
$ErrorActionPreference = 'Continue'
$out = $env:UAC_PROBE_OUT
$id  = [Security.Principal.WindowsIdentity]::GetCurrent()
$pr  = New-Object Security.Principal.WindowsPrincipal($id)
$lines = @(
  "whoami=$($id.Name)",
  "IsAdmin=$($pr.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator))",
  "sessionId=$((Get-Process -Id $PID).SessionId)"
)
# The payoff: from HERE, does Start-Process -Verb RunAs get a granted elevation?
$marker = Join-Path (Split-Path $out) 'elevated.txt'
try {
    $p = Start-Process -FilePath 'powershell.exe' -Verb RunAs -PassThru -WindowStyle Hidden -ArgumentList @(
        '-NoProfile','-Command',
        "`$i=[Security.Principal.WindowsIdentity]::GetCurrent(); `$r=New-Object Security.Principal.WindowsPrincipal(`$i); Set-Content -LiteralPath '$marker' -Value \"elevated_whoami=`$(`$i.Name)`nelevated_IsAdmin=`$(`$r.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator))\""
    )
    if ($p -and $p.WaitForExit(30000)) { $lines += "RunAs=launched exit=$($p.ExitCode)" }
    elseif ($p) { $lines += 'RunAs=launched but did not exit in 30s (a dialog?)'; try { $p.Kill() } catch {} }
    else { $lines += 'RunAs=Start-Process returned nothing' }
} catch {
    $lines += "RunAs=THREW $($_.Exception.Message)"
}
Set-Content -LiteralPath $out -Value $lines
'@ | Set-Content -LiteralPath $childPs -Encoding ASCII

if ($NoLaunch) {
    Say '  skipped: -NoLaunch (local interop check only)'
} elseif ($typeOk -and $linked -ne [IntPtr]::Zero) {
    $env:UAC_PROBE_OUT = $childOut
    $cmd = "powershell.exe -NoProfile -ExecutionPolicy Bypass -File `"$childPs`""
    $err = 0
    $pid2 = [UacProbe]::LaunchWith($linked, $cmd, $Work, [ref]$err)
    if ($pid2 -lt 0) {
        Say "  CreateProcessWithTokenW FAILED (lastError=$err)"
    } else {
        Say "  child pid = $pid2 ; waiting up to ${StepTimeoutSec}s for its report"
        $deadline = (Get-Date).AddSeconds($StepTimeoutSec)
        while ((Get-Date) -lt $deadline -and -not (Test-Path -LiteralPath $childOut)) { Start-Sleep -Milliseconds 500 }
        if (Test-Path -LiteralPath $childOut) {
            foreach ($l in (Get-Content -LiteralPath $childOut)) { Say "    child: $l" }
        } else {
            Say '    child: NO REPORT within the deadline'
        }
        $elev = Join-Path $Work 'elevated.txt'
        if (Test-Path -LiteralPath $elev) {
            foreach ($l in (Get-Content -LiteralPath $elev)) { Say "    GRANTED ELEVATION: $l" }
        } else {
            Say '    no elevated grandchild marker -- the elevation did not complete'
        }
    }
} else {
    Say '  skipped: no linked token to launch with'
}

Head 'verdict inputs'
Say "  elevationType=$elevType linkedToken=$(if ($linked -eq [IntPtr]::Zero) { 'none' } else { 'present' })"
Say "  work dir: $Work"

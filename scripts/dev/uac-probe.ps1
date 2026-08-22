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

# Event logs are read at the end for the window this probe covers.
$probeStart = Get-Date
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

    // Every launcher used the literal "winsta0\\default" while the DACL was
    // written to whatever GetProcessWindowStation() returned. Those are the
    // same object only by assumption, and the assumption is what this round is
    // testing, so the caller names it once and both agree by construction.
    public static string Desktop = "winsta0\\default";

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
    [DllImport("advapi32.dll", SetLastError=true, CharSet=CharSet.Unicode)]
    public static extern bool LogonUser(string user, string domain, string pass, int logonType, int provider, out IntPtr token);

    // Elevation facts for an ARBITRARY token. This is what separates "this
    // process is not split" from "this machine does not split at all".
    public static int ElevationTypeOf(IntPtr tok, out int lastError) {
        lastError = 0;
        IntPtr buf = Marshal.AllocHGlobal(4);
        try {
            int need;
            if (!GetTokenInformation(tok, TokenElevationType, buf, 4, out need)) { lastError = Marshal.GetLastWin32Error(); return 0; }
            return Marshal.ReadInt32(buf);
        } finally { Marshal.FreeHGlobal(buf); }
    }
    public static IntPtr LinkedTokenOf(IntPtr tok, out int lastError) {
        lastError = 0;
        IntPtr buf = Marshal.AllocHGlobal(IntPtr.Size);
        try {
            int need;
            if (!GetTokenInformation(tok, TokenLinkedToken, buf, IntPtr.Size, out need)) { lastError = Marshal.GetLastWin32Error(); return IntPtr.Zero; }
            return Marshal.ReadIntPtr(buf);
        } finally { Marshal.FreeHGlobal(buf); }
    }
    public static bool IsElevated(IntPtr tok, out int lastError) {
        lastError = 0;
        IntPtr buf = Marshal.AllocHGlobal(4);
        try {
            int need;
            if (!GetTokenInformation(tok, TokenElevation, buf, 4, out need)) { lastError = Marshal.GetLastWin32Error(); return false; }
            return Marshal.ReadInt32(buf) != 0;
        } finally { Marshal.FreeHGlobal(buf); }
    }

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

    [DllImport("user32.dll", SetLastError=true)] public static extern IntPtr GetProcessWindowStation();
    [DllImport("user32.dll", SetLastError=true)] public static extern IntPtr GetThreadDesktop(int threadId);
    [DllImport("kernel32.dll")] public static extern int GetCurrentThreadId();
    [DllImport("user32.dll", SetLastError=true)]
    public static extern bool GetUserObjectSecurity(IntPtr obj, ref int si, byte[] sd, int len, out int needed);
    [DllImport("user32.dll", SetLastError=true)]
    public static extern bool SetUserObjectSecurity(IntPtr obj, ref int si, byte[] sd);
    [DllImport("kernel32.dll", SetLastError=true)]
    public static extern bool GetExitCodeProcess(IntPtr h, out int code);
    [DllImport("kernel32.dll", SetLastError=true)]
    public static extern int WaitForSingleObject(IntPtr h, int ms);
    // The environment the elevated child is GIVEN is the subject of #164/#192
    // and of waired-agent#997 itself -- AppInfo builds one with exactly this
    // API. Here it matters for a duller reason: CreateProcessWithTokenW with
    // lpEnvironment = NULL hands the child the CALLER's environment, so a
    // second user inherits runneradmin's %TEMP% and %LOCALAPPDATA%, which it
    // cannot write. cmd does not care; powershell writes its module-analysis
    // cache there at startup.
    [DllImport("userenv.dll", SetLastError=true)]
    public static extern bool CreateEnvironmentBlock(out IntPtr env, IntPtr token, bool inherit);
    [DllImport("userenv.dll", SetLastError=true)]
    public static extern bool DestroyEnvironmentBlock(IntPtr env);


    // The window station and desktop being modified were never NAMED. A handle
    // from GetProcessWindowStation() is whatever this process happens to be on,
    // and granting the wrong object reports success just as loudly.
    [DllImport("user32.dll", SetLastError=true, CharSet=CharSet.Unicode)]
    public static extern bool GetUserObjectInformation(IntPtr obj, int index, System.Text.StringBuilder info, int len, out int needed);
    public static string ObjectName(IntPtr obj) {
        System.Text.StringBuilder sb = new System.Text.StringBuilder(512);
        int n;
        if (!GetUserObjectInformation(obj, 2 /* UOI_NAME */, sb, 512, out n)) { return "(name unavailable, err " + Marshal.GetLastWin32Error() + ")"; }
        return sb.ToString();
    }

    [StructLayout(LayoutKind.Sequential)]
    public struct SID_AND_ATTRIBUTES { public IntPtr Sid; public uint Attributes; }
    public const int TokenGroups = 2;
    public const uint SE_GROUP_LOGON_ID = 0xC0000000;
    [DllImport("advapi32.dll", SetLastError=true, CharSet=CharSet.Unicode)]
    public static extern bool ConvertSidToStringSid(IntPtr sid, out IntPtr str);
    [DllImport("kernel32.dll")] public static extern IntPtr LocalFree(IntPtr p);

    // Windows checks window station / desktop access against the token's LOGON
    // SID, which is minted per logon session and is NOT the user SID. Every
    // Microsoft sample for running a process on WinSta0\Default as another user
    // grants the logon SID; granting only the user SID is the likely reason a
    // DACL write that "succeeded" changed nothing.
    public static string LogonSidOf(IntPtr token, out int lastError) {
        lastError = 0;
        int need = 0;
        GetTokenInformation(token, TokenGroups, IntPtr.Zero, 0, out need);
        if (need <= 0) { lastError = Marshal.GetLastWin32Error(); return null; }
        IntPtr buf = Marshal.AllocHGlobal(need);
        try {
            if (!GetTokenInformation(token, TokenGroups, buf, need, out need)) { lastError = Marshal.GetLastWin32Error(); return null; }
            int count = Marshal.ReadInt32(buf);
            int stride = Marshal.SizeOf(typeof(SID_AND_ATTRIBUTES));
            long start = buf.ToInt64() + IntPtr.Size; // GroupCount then padding to pointer alignment
            for (int i = 0; i < count; i++) {
                SID_AND_ATTRIBUTES sa = (SID_AND_ATTRIBUTES)Marshal.PtrToStructure(new IntPtr(start + (long)i * stride), typeof(SID_AND_ATTRIBUTES));
                if ((sa.Attributes & SE_GROUP_LOGON_ID) == SE_GROUP_LOGON_ID) {
                    IntPtr str;
                    if (!ConvertSidToStringSid(sa.Sid, out str)) { lastError = Marshal.GetLastWin32Error(); return null; }
                    string s = Marshal.PtrToStringUni(str);
                    LocalFree(str);
                    return s;
                }
            }
            return null;
        } finally { Marshal.FreeHGlobal(buf); }
    }

    // TOKEN_STATISTICS: LUID TokenId, then LUID AuthenticationId. A logon SID
    // is S-1-5-5-<AuthenticationId.HighPart>-<LowPart>, so this derives the same
    // value a completely different way and the two can be compared. Locally
    // they DISAGREE for a PowerShell launched through the WSL interop bridge,
    // which is why the probe prints both rather than picking one.
    public const int TokenStatistics = 10;
    public static string LogonSidFromStatistics(IntPtr token, out int lastError) {
        lastError = 0;
        IntPtr buf = Marshal.AllocHGlobal(128);
        try {
            int need;
            if (!GetTokenInformation(token, TokenStatistics, buf, 128, out need)) { lastError = Marshal.GetLastWin32Error(); return null; }
            int low  = Marshal.ReadInt32(buf, 8);
            int high = Marshal.ReadInt32(buf, 12);
            return "S-1-5-5-" + high + "-" + low;
        } finally { Marshal.FreeHGlobal(buf); }
    }

    // Read a window station's / desktop's DACL as a binary security descriptor.
    // Returned to PowerShell so RawSecurityDescriptor can do the ACE editing --
    // hand-rolling ACL structs here would be far more code for no more truth.
    public static byte[] GetDacl(IntPtr obj, out int lastError) {
        lastError = 0;
        int si = 4; // DACL_SECURITY_INFORMATION
        int needed;
        GetUserObjectSecurity(obj, ref si, new byte[0], 0, out needed);
        byte[] sd = new byte[needed];
        if (!GetUserObjectSecurity(obj, ref si, sd, needed, out needed)) { lastError = Marshal.GetLastWin32Error(); return null; }
        return sd;
    }
    public static bool SetDacl(IntPtr obj, byte[] sd, out int lastError) {
        lastError = 0;
        int si = 4;
        if (!SetUserObjectSecurity(obj, ref si, sd)) { lastError = Marshal.GetLastWin32Error(); return false; }
        return true;
    }

    // Launch, wait, and report the exit code -- "the process was created" and
    // "the process ran" are different claims, and P4 could not tell them apart.
    // waitResult is WaitForSingleObject's own answer: 0 = the process exited,
    // 258 = WAIT_TIMEOUT. Without it, "exit code 259" is ambiguous between
    // "still running" and "the wait itself failed".
    public static int LaunchAndWait(IntPtr token, string cmdLine, string dir, int waitMs, uint logonFlags,
                                    uint creationFlags, bool userEnv, out int exitCode, out int waitResult, out int lastError) {
        exitCode = -1; waitResult = -1; lastError = 0;
        IntPtr primary;
        if (!DuplicateTokenEx(token, MAXIMUM_ALLOWED, IntPtr.Zero, 2, 1, out primary)) { lastError = Marshal.GetLastWin32Error(); return -1; }
        IntPtr env = IntPtr.Zero;
        try {
            if (userEnv) {
                if (!CreateEnvironmentBlock(out env, primary, false)) { lastError = Marshal.GetLastWin32Error(); return -2; }
                creationFlags |= 0x00000400; // CREATE_UNICODE_ENVIRONMENT, required with a block from CreateEnvironmentBlock
            }
            STARTUPINFO si = new STARTUPINFO();
            si.cb = Marshal.SizeOf(si);
            si.lpDesktop = Desktop;
            PROCESS_INFORMATION pi;
            if (!CreateProcessWithTokenW(primary, logonFlags, null, cmdLine, creationFlags, env, dir, ref si, out pi)) {
                lastError = Marshal.GetLastWin32Error(); return -1;
            }
            waitResult = WaitForSingleObject(pi.hProcess, waitMs);
            GetExitCodeProcess(pi.hProcess, out exitCode);
            // A timed-out attempt must NOT outlive itself. Round 1 left a
            // blocked cmd alive holding the next attempt's redirect target
            // open, and cmd answers a failed redirect by SKIPPING the command
            // and leaving ERRORLEVEL at 0 -- which read as a pass. Measured.
            if (waitResult == 258) { TerminateProcess(pi.hProcess, 259); }
            CloseHandle(pi.hThread);
            CloseHandle(pi.hProcess);
            return pi.dwProcessId;
        } finally {
            if (env != IntPtr.Zero) { DestroyEnvironmentBlock(env); }
            CloseHandle(primary);
        }
    }


    [DllImport("kernel32.dll", SetLastError=true)] public static extern bool TerminateProcess(IntPtr h, uint code);

    [StructLayout(LayoutKind.Sequential)] public struct LUID { public uint Low; public int High; }
    [StructLayout(LayoutKind.Sequential)] public struct LUID_AND_ATTRIBUTES { public LUID Luid; public uint Attributes; }
    [StructLayout(LayoutKind.Sequential)] public struct TOKEN_PRIVILEGES { public uint Count; public LUID_AND_ATTRIBUTES Priv; }
    public const uint TOKEN_ADJUST_PRIVILEGES = 0x0020;
    [DllImport("advapi32.dll", SetLastError=true, CharSet=CharSet.Unicode)]
    public static extern bool LookupPrivilegeValue(string sys, string name, out LUID luid);
    [DllImport("advapi32.dll", SetLastError=true)]
    public static extern bool AdjustTokenPrivileges(IntPtr token, bool disableAll, ref TOKEN_PRIVILEGES newState, int len, IntPtr prev, IntPtr retLen);

    // Process.Modules on ANOTHER user's process needs PROCESS_VM_READ, which an
    // administrator only gets with SeDebugPrivilege actually ENABLED. It is
    // present in the token but off by default and .NET never turns it on, so
    // without this the module list -- the whole point of watching the hang --
    // comes back "Access is denied" and looks like a different problem.
    public static bool EnablePrivilege(string name, out int lastError) {
        lastError = 0;
        IntPtr tok;
        if (!OpenProcessToken(GetCurrentProcess(), TOKEN_ADJUST_PRIVILEGES | TOKEN_QUERY, out tok)) { lastError = Marshal.GetLastWin32Error(); return false; }
        try {
            LUID luid;
            if (!LookupPrivilegeValue(null, name, out luid)) { lastError = Marshal.GetLastWin32Error(); return false; }
            TOKEN_PRIVILEGES tp = new TOKEN_PRIVILEGES();
            tp.Count = 1; tp.Priv.Luid = luid; tp.Priv.Attributes = 0x00000002; // SE_PRIVILEGE_ENABLED
            if (!AdjustTokenPrivileges(tok, false, ref tp, Marshal.SizeOf(tp), IntPtr.Zero, IntPtr.Zero)) { lastError = Marshal.GetLastWin32Error(); return false; }
            // AdjustTokenPrivileges returns TRUE having assigned nothing; the
            // only tell is ERROR_NOT_ALL_ASSIGNED (1300) in the last error.
            lastError = Marshal.GetLastWin32Error();
            return lastError == 0;
        } finally { CloseHandle(tok); }
    }

    // Start and DO NOT wait. The hang itself is the subject now, so the process
    // has to stay alive and reachable while it is being looked at; every
    // earlier variant blocked inside the call and could only report a number.
    public static int LaunchDetached(IntPtr token, string cmdLine, string dir, uint logonFlags,
                                     uint creationFlags, bool userEnv, out IntPtr hProcess, out int lastError) {
        hProcess = IntPtr.Zero; lastError = 0;
        IntPtr primary;
        if (!DuplicateTokenEx(token, MAXIMUM_ALLOWED, IntPtr.Zero, 2, 1, out primary)) { lastError = Marshal.GetLastWin32Error(); return -1; }
        IntPtr env = IntPtr.Zero;
        try {
            if (userEnv) {
                if (!CreateEnvironmentBlock(out env, primary, false)) { lastError = Marshal.GetLastWin32Error(); return -2; }
                creationFlags |= 0x00000400; // CREATE_UNICODE_ENVIRONMENT
            }
            STARTUPINFO si = new STARTUPINFO();
            si.cb = Marshal.SizeOf(si);
            si.lpDesktop = Desktop;
            PROCESS_INFORMATION pi;
            if (!CreateProcessWithTokenW(primary, logonFlags, null, cmdLine, creationFlags, env, dir, ref si, out pi)) {
                lastError = Marshal.GetLastWin32Error(); return -1;
            }
            CloseHandle(pi.hThread);
            hProcess = pi.hProcess;
            return pi.dwProcessId;
        } finally {
            if (env != IntPtr.Zero) { DestroyEnvironmentBlock(env); }
            CloseHandle(primary);
        }
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
            si.lpDesktop = Desktop;
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

# --------------------------------------------------------------------------
Head 'P3  a SECOND local admin (not RID 500), logged on INTERACTIVELY'
# --------------------------------------------------------------------------
# The runner logs on as RID 500 with Admin Approval Mode off, so IT has no
# split token -- a property of the ACCOUNT, not of hosted runners. A
# non-RID-500 administrator should be split at interactive logon. The earlier
# scheduled-task attempt (waired-agent#997) failed because a BATCH logon is
# never filtered, which is a different reason and left this untested.
#
# LocalAccountTokenFilterPolicy=1 is set on this image and its documented scope
# is NETWORK logons -- so whether it also defeats an interactive one is exactly
# the thing to measure rather than reason about.
$LOGON32_LOGON_INTERACTIVE = 2
$LOGON32_PROVIDER_DEFAULT  = 0
$u  = 'waired-uacprobe'
$pw = 'Pr0be-' + [guid]::NewGuid().ToString('N').Substring(0, 16) + '!aZ'

if (-not $typeOk -or $NoLaunch) {
    Say '  skipped (no interop, or -NoLaunch)'
} else {
    $made = $false
    try {
        # New-LocalUser, not `net user`: this is what installtest-windows.ps1
        # already uses successfully on this image (:1656). The first attempt
        # here used `net user` AND piped its output to Out-Null, so when the
        # create failed the only symptom was a SID that would not translate --
        # the diagnostic had been thrown away. Errors are shown now.
        $sec = ConvertTo-SecureString $pw -AsPlainText -Force
        New-LocalUser -Name $u -Password $sec -PasswordNeverExpires -AccountNeverExpires -ErrorAction Stop | Out-Null
        Add-LocalGroupMember -Group 'Administrators' -Member $u -ErrorAction Stop
        $made = $true
        $sid = (Get-LocalUser -Name $u).SID.Value
        Say "  created $u  SID=$sid  is-RID-500=$($sid.EndsWith('-500'))"
        $inAdmins = @(Get-LocalGroupMember -Group 'Administrators' | Where-Object { $_.Name -like "*\$u" }).Count -gt 0
        Say "  in Administrators = $inAdmins"

        $tok = [IntPtr]::Zero
        $ok = [UacProbe]::LogonUser($u, '.', $pw, $LOGON32_LOGON_INTERACTIVE, $LOGON32_PROVIDER_DEFAULT, [ref]$tok)
        if (-not $ok) {
            Say "  LogonUser(INTERACTIVE) FAILED lastError=$([Runtime.InteropServices.Marshal]::GetLastWin32Error())"
        } else {
            Say '  LogonUser(INTERACTIVE) ok'
            $names = @{ 0 = 'ERROR'; 1 = 'Default (NO split token)'; 2 = 'Full'; 3 = 'Limited (FILTERED -- what #997 needs)' }
            $err = 0
            $t = [UacProbe]::ElevationTypeOf($tok, [ref]$err)
            Say "    TokenElevationType = $t  $($names[$t])  (lastError=$err)"
            $err = 0
            Say "    TokenElevation(isElevated) = $([UacProbe]::IsElevated($tok, [ref]$err))"
            $err = 0
            $lnk = [UacProbe]::LinkedTokenOf($tok, [ref]$err)
            if ($lnk -eq [IntPtr]::Zero) {
                Say "    TokenLinkedToken = NONE (lastError=$err)"
            } else {
                $err2 = 0
                $lt = [UacProbe]::ElevationTypeOf($lnk, [ref]$err2)
                Say "    TokenLinkedToken = present; its ElevationType = $lt  $($names[$lt])"
            }

            # ------------------------------------------------------------------
            Head 'P4  run as that FILTERED admin, and ask AppInfo to elevate it'
            # ------------------------------------------------------------------
            # The payoff. A Limited token is only interesting if a process
            # holding it can reach Start-Process -Verb RunAs and get a GRANTED
            # elevation -- which is the single thing waired-agent#997 says is
            # never executed. ConsentPromptBehaviorAdmin is already 0 on this
            # image, so there should be no UI to answer.
            #
            # Shared scratch under C:\Users\Public: the new user has no access
            # to runneradmin's temp, and a child that cannot write its report
            # looks exactly like a child that never ran.
            if ($t -ne 3) {
                Say '  skipped: the token is not Limited, so there is nothing to elevate FROM'
            } else {
                $pub = 'C:\Users\Public\uac-probe'
                New-Item -ItemType Directory -Path $pub -Force | Out-Null
                & icacls.exe $pub /grant "${u}:(OI)(CI)F" 2>&1 | Out-Null
                $rep  = Join-Path $pub 'child.txt'
                $mark = Join-Path $pub 'elevated.txt'
                Remove-Item -LiteralPath $rep, $mark -ErrorAction SilentlyContinue

                # BREADCRUMBS, appended as it goes. The previous round wrote a
                # single report at the very end, so a hang anywhere -- in
                # PowerShell's own startup, or inside Start-Process -Verb RunAs
                # -- produced the identical symptom: an empty file. Each step
                # records itself before attempting the next one.
                $inner = @"
function Note(`$m) { Add-Content -LiteralPath '$rep' -Value `$m }
Note "01 powershell started"
`$i = [Security.Principal.WindowsIdentity]::GetCurrent()
`$r = New-Object Security.Principal.WindowsPrincipal(`$i)
Note "02 whoami=`$(`$i.Name)"
Note "03 IsAdmin=`$(`$r.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator))"
Note "04 about to call Start-Process -Verb RunAs"
try {
    # No inner quoting around the path: `\`" inside this here-string renders a
    # bare " that closes the argument string early. The rendered line still
    # PARSES, so the only symptom on the runner would have been a wrong
    # argument -- caught by rendering the child locally and reading it. The
    # path lives under C:\Users\Public\uac-probe and has no spaces.
    `$p = Start-Process -FilePath 'cmd.exe' -Verb RunAs -PassThru -WindowStyle Hidden ``
            -ArgumentList '/c', 'echo elevated> $mark'
    Note "05 Start-Process returned (pid=`$(if (`$p) { `$p.Id } else { 'null' }))"
    if (`$p -and `$p.WaitForExit(20000)) { Note "06 elevated child exited `$(`$p.ExitCode)" }
    elseif (`$p) { Note '06 elevated child did not exit in 20s'; try { `$p.Kill() } catch {} }
    else { Note '06 Start-Process returned nothing' }
} catch { Note "05E Start-Process THREW: `$(`$_.Exception.Message)" }
Note "07 done"
"@
                $childPs2 = Join-Path $pub 'child2.ps1'
                Set-Content -LiteralPath $childPs2 -Value $inner -Encoding ASCII
                & icacls.exe $childPs2 /grant "${u}:RX" 2>&1 | Out-Null

                # A -- the SMALLEST possible child, before any ACL work. The
                # previous round launched PowerShell and got silence, which
                # cannot distinguish "the process could not start" from
                # "PowerShell could not start" from "it could not write". cmd
                # writing one line separates all three, and the exit code says
                # whether it ran at all.
                $alive = Join-Path $pub 'alive.txt'
                # $ExpectExit is a fingerprint, not decoration: round 1 read
                # "the marker file exists" as success for a child that never
                # ran. Where a caller can name the exit code it wants, the run
                # only counts if the child produced exactly that.
                function Try-Child {
                    param([string]$Label, [IntPtr]$Token, [string]$Cmd, [string]$Marker = '',
                          [uint32]$LogonFlags = 0, [uint32]$CreationFlags = 0, [int]$WaitMs = 20000, [bool]$UserEnv = $false,
                          [int]$ExpectExit = -12345)
                    if ($Marker) { Remove-Item -LiteralPath $Marker -ErrorAction SilentlyContinue }
                    $ec = -1; $wr = -1; $e = 0
                    $p = [UacProbe]::LaunchAndWait($Token, $Cmd, $pub, $WaitMs, $LogonFlags, $CreationFlags, $UserEnv, [ref]$ec, [ref]$wr, [ref]$e)
                    if ($p -eq -2) { Say "  ${Label}: CreateEnvironmentBlock FAILED lastError=$e"; return $false }
                    if ($p -lt 0) {
                        Say "  ${Label}: CreateProcessWithTokenW FAILED lastError=$e (5=access denied, 1314=no SeImpersonate)"
                        return $false
                    }
                    $hex  = '0x{0:X8}' -f $ec
                    $wtxt = switch ($wr) { 0 { 'WAIT_OBJECT_0 (it exited)' } 258 { 'WAIT_TIMEOUT (still running)' } default { "wait=$wr" } }
                    $note = switch ($ec) {
                        259         { '  <- STILL_ACTIVE' }
                        -1073741502 { '  <- STATUS_DLL_INIT_FAILED: no window station/desktop' }
                        default     { '' }
                    }
                    Say "  ${Label}: pid=$p exit=$ec ($hex) $wtxt$note"
                    $exitOk = ($ExpectExit -eq -12345) -or ($ec -eq $ExpectExit)
                    if (-not $exitOk) { Say "    ${Label}: expected exit $ExpectExit, got $ec" }
                    if (-not $Marker) { return $exitOk }
                    if (Test-Path -LiteralPath $Marker) {
                        foreach ($l in Get-Content -LiteralPath $Marker) { Say "    ${Label} said: $l" }
                        return $exitOk
                    }
                    Say "    ${Label}: wrote nothing"
                    return $false
                }

                # ----------------------------------------------------------
                # Round 3. Rounds 1-2 measured the cause instead of guessing
                # at it, and it is not what the earlier rounds assumed:
                #
                #   * the child is not hanging. It fails in LOADER INIT and
                #     then blocks in NtRaiseHardError forever, because a hard
                #     error wants a dialog dismissed and nothing in this
                #     session can dismiss it (cdb stack, run 32590784862).
                #   * the status is 0xC0000142 STATUS_DLL_INIT_FAILED, read
                #     twice: out of the hard error's arguments, and as an exit
                #     code once hard errors are suppressed (run 32591552734).
                #   * it is NOT about PowerShell. whoami.exe fails identically
                #     and pwsh 7 fails identically. cmd.exe is the only thing
                #     that runs -- and the failing module list carries USER32,
                #     win32u, GDI32 and IMM32, which cmd does not load.
                #
                # user32's DllMain connects to the window station and desktop.
                # Denied there, every process that loads it dies exactly this
                # way. So the DACL grant that reported success last round did
                # not actually grant what Windows checks -- and what Windows
                # checks is the token's LOGON SID, minted per logon session,
                # not the user SID.
                # ----------------------------------------------------------
                $PS51 = 'C:\Windows\System32\WindowsPowerShell\v1.0\powershell.exe'
                $hWinsta = [UacProbe]::GetProcessWindowStation()
                $hDesk   = [UacProbe]::GetThreadDesktop([UacProbe]::GetCurrentThreadId())
                $winstaName = [UacProbe]::ObjectName($hWinsta)
                $deskName   = [UacProbe]::ObjectName($hDesk)
                Say "  window station = $winstaName"
                Say "  desktop        = $deskName"
                # Name the target once so the grant and the launch cannot drift.
                [UacProbe]::Desktop = "$winstaName\$deskName"
                Say "  children will be started on $([UacProbe]::Desktop)"

                $e = 0; $logonSid = [UacProbe]::LogonSidOf($tok, [ref]$e)
                $e2 = 0; $authSid = [UacProbe]::LogonSidFromStatistics($tok, [ref]$e2)
                Say "  logon SID via TokenGroups      = $(if ($logonSid) { $logonSid } else { "(none, lastError=$e)" })"
                Say "  logon SID via AuthenticationId = $(if ($authSid) { $authSid } else { "(none, lastError=$e2)" })"
                Say "  the two derivations agree      = $($logonSid -eq $authSid)"

                # Grant, then RE-READ. "SetUserObjectSecurity returned true" and
                # "the ACE is on the object" are different claims, and last
                # round only ever checked the first one.
                function Grant-Object {
                    param([IntPtr]$Handle, [string]$Name, [string[]]$Sids, [bool]$IsWinsta)
                    $err = 0
                    $raw = [UacProbe]::GetDacl($Handle, [ref]$err)
                    if (-not $raw) { Say "  cannot read the $Name DACL (lastError=$err)"; return $false }
                    $rsd = New-Object Security.AccessControl.RawSecurityDescriptor($raw, 0)
                    foreach ($s in $Sids) {
                        $aceSid = New-Object Security.Principal.SecurityIdentifier($s)
                        if ($IsWinsta) {
                            # The documented pair: one INHERIT_ONLY ace that the
                            # desktops under this station inherit, and one
                            # non-inherited ace for the station object itself.
                            # An INHERIT_ONLY ace grants nothing on the object
                            # it sits on, which is why one of them is not enough.
                            $f1 = [Security.AccessControl.AceFlags]'ObjectInherit, ContainerInherit, NoPropagateInherit, InheritOnly'
                            $rsd.DiscretionaryAcl.InsertAce(0, (New-Object Security.AccessControl.CommonAce(
                                $f1, [Security.AccessControl.AceQualifier]::AccessAllowed, 0x10000000, $aceSid, $false, $null)))
                            $f2 = [Security.AccessControl.AceFlags]'NoPropagateInherit'
                            $rsd.DiscretionaryAcl.InsertAce(0, (New-Object Security.AccessControl.CommonAce(
                                $f2, [Security.AccessControl.AceQualifier]::AccessAllowed, 0x0000037F, $aceSid, $false, $null)))
                        } else {
                            $rsd.DiscretionaryAcl.InsertAce(0, (New-Object Security.AccessControl.CommonAce(
                                [Security.AccessControl.AceFlags]::None, [Security.AccessControl.AceQualifier]::AccessAllowed,
                                0x000001FF, $aceSid, $false, $null)))
                        }
                    }
                    $buf = New-Object byte[] $rsd.BinaryLength
                    $rsd.GetBinaryForm($buf, 0)
                    $err = 0
                    if (-not [UacProbe]::SetDacl($Handle, $buf, [ref]$err)) {
                        Say "  SetDacl on the $Name FAILED (lastError=$err)"; return $false
                    }
                    $err = 0
                    $after = [UacProbe]::GetDacl($Handle, [ref]$err)
                    if (-not $after) { Say "  wrote the $Name DACL but cannot re-read it (lastError=$err)"; return $false }
                    $rsd2 = New-Object Security.AccessControl.RawSecurityDescriptor($after, 0)
                    $have = @($rsd2.DiscretionaryAcl |
                              Where-Object { $_ -is [Security.AccessControl.CommonAce] } |
                              ForEach-Object { $_.SecurityIdentifier.Value })
                    $all = $true
                    foreach ($s in $Sids) {
                        $ok = $have -contains $s
                        if (-not $ok) { $all = $false }
                        Say "    $Name : $s present after write = $ok"
                    }
                    return $all
                }

                # The user SID stays in the list: it is what the previous round
                # granted, so keeping it makes this a strict addition and the
                # re-read says which of them the object actually carries.
                $grantSids = @($sid, $logonSid, $authSid) | Where-Object { $_ } | Select-Object -Unique
                Say "  granting: $($grantSids -join ' ')"
                $gW = Grant-Object -Handle $hWinsta -Name 'window station' -Sids $grantSids -IsWinsta $true
                $gD = Grant-Object -Handle $hDesk   -Name 'desktop'        -Sids $grantSids -IsWinsta $false
                Say "  grants verified: window station=$gW desktop=$gD"

                # Hard errors stay suppressed for the whole block. A loader
                # failure then EXITS with its status instead of blocking on a
                # dialog, which is what turned a 90-second mystery into a
                # number -- and it keeps every step below bounded.
                $emKey = 'HKLM:\SYSTEM\CurrentControlSet\Control\Windows'
                $emOld = try { (Get-ItemProperty -LiteralPath $emKey -Name ErrorMode -ErrorAction Stop).ErrorMode } catch { $null }
                try {
                    Set-ItemProperty -LiteralPath $emKey -Name ErrorMode -Value 2 -Type DWord -ErrorAction Stop
                    Say "  hard errors suppressed (ErrorMode 2, was $(if ($null -eq $emOld) { 'absent' } else { $emOld }))"

                    # E1 -- the cheapest possible indicator. whoami.exe loads
                    # user32 and nothing else interesting, so it answers the
                    # window-station question on its own.
                    $e1 = Try-Child -Label 'E1 whoami.exe' -Token $tok -Cmd 'C:\Windows\System32\whoami.exe' `
                            -CreationFlags 0x08000000 -WaitMs 30000 -ExpectExit 0
                    Say "  E1 says the window station is $(if ($e1) { 'REACHABLE' } else { 'still refused' })"

                    $ps51Done = Join-Path $pub 'ps51.done'
                    $e3 = Try-Child -Label 'E3 powershell 5.1' -Token $tok `
                            -Cmd "$PS51 -NoProfile -NonInteractive -Command `"Set-Content -LiteralPath '$ps51Done' -Value ok; exit 7`"" `
                            -Marker $ps51Done -CreationFlags 0x08000000 -WaitMs 90000 -UserEnv $true -ExpectExit 7

                    if (-not $e3) {
                        Say '  powershell still does not start; the payoff below is not attempted'
                    } else {
                        # THE PAYOFF. A filtered admin that can run PowerShell
                        # can be asked the one question waired-agent#997 says
                        # is never executed: does Start-Process -Verb RunAs
                        # get a GRANTED elevation without a human?
                        Say '  powershell 5.1 RUNS as the filtered admin -- asking for the elevation'
                        [void](Try-Child -Label 'E5 child2.ps1 -> Start-Process -Verb RunAs' -Token $tok `
                                -Cmd "$PS51 -NoProfile -NonInteractive -ExecutionPolicy Bypass -File `"$childPs2`"" `
                                -Marker $rep -CreationFlags 0x08000000 -WaitMs 120000 -UserEnv $true)
                        foreach ($f in @($rep, $mark)) {
                            if (Test-Path -LiteralPath $f) {
                                foreach ($l in (Get-Content -LiteralPath $f)) { Say "    $(Split-Path $f -Leaf): $l" }
                            } else { Say "    $(Split-Path $f -Leaf): absent" }
                        }
                        if (Test-Path -LiteralPath $mark) {
                            Say '  *** a GRANTED UAC elevation ran with no human present ***'
                        }
                    }
                } catch {
                    Say "  round 3 threw: $($_.Exception.Message)"
                } finally {
                    if ($null -eq $emOld) { Remove-ItemProperty -LiteralPath $emKey -Name ErrorMode -ErrorAction SilentlyContinue }
                    else { Set-ItemProperty -LiteralPath $emKey -Name ErrorMode -Value $emOld -Type DWord -ErrorAction SilentlyContinue }
                    Say '  ErrorMode restored'
                }

                Say "  profile dir for ${u} exists (after) = $(Test-Path -LiteralPath "C:\Users\$u")"
                Remove-Item -LiteralPath $pub -Recurse -Force -ErrorAction SilentlyContinue
            }
        }
    } catch {
        Say "  P3 threw: $($_.Exception.Message)"
    } finally {
        if ($made) {
            try { Remove-LocalUser -Name $u -ErrorAction Stop; Say "  removed $u" }
            catch { Say "  could not remove ${u}: $($_.Exception.Message)" }
        }
    }
}

Head 'verdict inputs'
Say "  P1 route (borrow this process's linked token): elevationType=$elevType linkedToken=$(if ($linked -eq [IntPtr]::Zero) { 'none' } else { 'present' })"
Say '  P3 route (second admin, interactive logon)   : see above'
Say "  work dir: $Work"

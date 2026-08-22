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

    // CreateProcessWithTokenW documents the rights the token needs:
    // TOKEN_ASSIGN_PRIMARY | TOKEN_DUPLICATE | TOKEN_QUERY (+ ADJUST_DEFAULT
    // and ADJUST_SESSIONID). MAXIMUM_ALLOWED gives whatever the source token
    // happens to allow, which for a LINKED token was not enough -- the call
    // came back 1346 ERROR_BAD_IMPERSONATION_LEVEL rather than a result.
    public const uint TOKEN_RIGHTS_FOR_CREATEPROCESS = 0x0001 | 0x0002 | 0x0008 | 0x0080 | 0x0100;
    public static uint DupAccess = MAXIMUM_ALLOWED;

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

    // LABEL_SECURITY_INFORMATION. The mandatory label is evaluated BEFORE the
    // DACL, so a desktop labelled above the caller refuses it no matter who is
    // granted what -- which is the shape of "Everyone changed nothing", now
    // that the lpDesktop confound is gone.
    public static byte[] GetLabel(IntPtr obj, out int lastError) {
        lastError = 0;
        int si = 0x00000010;
        int needed;
        GetUserObjectSecurity(obj, ref si, new byte[0], 0, out needed);
        if (needed <= 0) { lastError = Marshal.GetLastWin32Error(); return null; }
        byte[] sd = new byte[needed];
        int si2 = 0x00000010;
        if (!GetUserObjectSecurity(obj, ref si2, sd, needed, out needed)) { lastError = Marshal.GetLastWin32Error(); return null; }
        return sd;
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
        if (!DuplicateTokenEx(token, DupAccess, IntPtr.Zero, 2, 1, out primary)) { lastError = Marshal.GetLastWin32Error(); return -1; }
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

    // A UAC-filtered administrator runs at MEDIUM integrity; this runner's own
    // account is RID 500 with Admin Approval Mode off, so everything it starts
    // runs at HIGH. Window stations and desktops carry a mandatory label, and
    // the mandatory check is made BEFORE the DACL -- which would explain why
    // granting Everyone changed nothing.
    public const int TokenIntegrityLevel = 25;
    public static string IntegrityOf(IntPtr token, out int lastError) {
        lastError = 0;
        int need = 0;
        GetTokenInformation(token, TokenIntegrityLevel, IntPtr.Zero, 0, out need);
        if (need <= 0) { lastError = Marshal.GetLastWin32Error(); return null; }
        IntPtr buf = Marshal.AllocHGlobal(need);
        try {
            if (!GetTokenInformation(token, TokenIntegrityLevel, buf, need, out need)) { lastError = Marshal.GetLastWin32Error(); return null; }
            IntPtr sid = Marshal.ReadIntPtr(buf); // TOKEN_MANDATORY_LABEL.Label.Sid
            IntPtr str;
            if (!ConvertSidToStringSid(sid, out str)) { lastError = Marshal.GetLastWin32Error(); return null; }
            string s = Marshal.PtrToStringUni(str);
            LocalFree(str);
            return s;
        } finally { Marshal.FreeHGlobal(buf); }
    }

    public static IntPtr OwnToken(out int lastError) {
        lastError = 0;
        IntPtr tok;
        if (!OpenProcessToken(GetCurrentProcess(), TOKEN_QUERY | TOKEN_DUPLICATE, out tok)) { lastError = Marshal.GetLastWin32Error(); return IntPtr.Zero; }
        return tok;
    }

    // Every round so far has launched into a logon session that has NO user
    // profile -- C:\Users\<user> never appears and HKCU has no hive. That is
    // the one anomaly common to all of them, and loading the profile is the
    // caller's job: seclogon only does it when asked, and it can fail quietly.
    // Needs SeRestore and SeBackup enabled.
    [StructLayout(LayoutKind.Sequential, CharSet=CharSet.Unicode)]
    public struct PROFILEINFO {
        public int dwSize; public int dwFlags;
        public string lpUserName, lpProfilePath, lpDefaultPath, lpServerName, lpPolicyPath;
        public IntPtr hProfile;
    }
    [DllImport("userenv.dll", SetLastError=true, CharSet=CharSet.Unicode)]
    public static extern bool LoadUserProfile(IntPtr token, ref PROFILEINFO info);
    [DllImport("userenv.dll", SetLastError=true)]
    public static extern bool UnloadUserProfile(IntPtr token, IntPtr hProfile);

    public static IntPtr LoadProfile(IntPtr token, string user, out int lastError) {
        lastError = 0;
        PROFILEINFO pi = new PROFILEINFO();
        pi.dwSize = Marshal.SizeOf(typeof(PROFILEINFO));
        pi.lpUserName = user;
        pi.dwFlags = 1; // PI_NOUI
        if (!LoadUserProfile(token, ref pi)) { lastError = Marshal.GetLastWin32Error(); return IntPtr.Zero; }
        return pi.hProfile;
    }

    // CreateProcessWithLogonW is what `runas` itself uses. It needs no
    // privilege, and unlike everything tried so far it performs the LOGON
    // itself -- seclogon does the window station and desktop plumbing, the
    // profile, and the logon-session association internally, instead of this
    // probe assembling them by hand. Ten rounds of negative results are all
    // about that hand assembly, so the API that does it properly is the
    // control none of them had.
    [DllImport("advapi32.dll", SetLastError=true, CharSet=CharSet.Unicode)]
    public static extern bool CreateProcessWithLogonW(string user, string domain, string pass, uint logonFlags,
        string app, string cmd, uint flags, IntPtr env, string dir, ref STARTUPINFO si, out PROCESS_INFORMATION pi);

    public static int LaunchWithLogonAndWait(string user, string domain, string pass, uint logonFlags,
            string cmdLine, string dir, int waitMs, uint creationFlags,
            out int exitCode, out int waitResult, out int lastError) {
        exitCode = -1; waitResult = -1; lastError = 0;
        STARTUPINFO si = new STARTUPINFO();
        si.cb = Marshal.SizeOf(si);
        si.lpDesktop = Desktop;
        PROCESS_INFORMATION pi;
        if (!CreateProcessWithLogonW(user, domain, pass, logonFlags, null, cmdLine, creationFlags, IntPtr.Zero, dir, ref si, out pi)) {
            lastError = Marshal.GetLastWin32Error(); return -1;
        }
        waitResult = WaitForSingleObject(pi.hProcess, waitMs);
        GetExitCodeProcess(pi.hProcess, out exitCode);
        if (waitResult == 258) { TerminateProcess(pi.hProcess, 259); }
        CloseHandle(pi.hThread);
        CloseHandle(pi.hProcess);
        return pi.dwProcessId;
    }

    // CreateProcessAsUser is a DIFFERENT mechanism, not a different flag:
    // CreateProcessWithTokenW hands the job to the Secondary Logon service by
    // RPC and the child is created by seclogon, whereas this creates it here,
    // directly. It is what services use to launch into a user's session. It
    // needs SeAssignPrimaryToken and SeIncreaseQuota actually ENABLED, which an
    // administrator holds but does not get switched on for free.
    [DllImport("advapi32.dll", SetLastError=true, CharSet=CharSet.Unicode)]
    public static extern bool CreateProcessAsUser(IntPtr token, string app, string cmd, IntPtr procAttr,
        IntPtr threadAttr, bool inheritHandles, uint flags, IntPtr env, string dir,
        ref STARTUPINFO si, out PROCESS_INFORMATION pi);

    public static int LaunchAsUserAndWait(IntPtr token, string cmdLine, string dir, int waitMs,
                                          uint creationFlags, bool userEnv, out int exitCode, out int waitResult, out int lastError) {
        exitCode = -1; waitResult = -1; lastError = 0;
        IntPtr primary;
        if (!DuplicateTokenEx(token, DupAccess, IntPtr.Zero, 2, 1, out primary)) { lastError = Marshal.GetLastWin32Error(); return -1; }
        IntPtr env = IntPtr.Zero;
        try {
            if (userEnv) {
                if (!CreateEnvironmentBlock(out env, primary, false)) { lastError = Marshal.GetLastWin32Error(); return -2; }
                creationFlags |= 0x00000400;
            }
            STARTUPINFO si = new STARTUPINFO();
            si.cb = Marshal.SizeOf(si);
            si.lpDesktop = Desktop;
            PROCESS_INFORMATION pi;
            if (!CreateProcessAsUser(primary, null, cmdLine, IntPtr.Zero, IntPtr.Zero, false, creationFlags, env, dir, ref si, out pi)) {
                lastError = Marshal.GetLastWin32Error(); return -1;
            }
            waitResult = WaitForSingleObject(pi.hProcess, waitMs);
            GetExitCodeProcess(pi.hProcess, out exitCode);
            if (waitResult == 258) { TerminateProcess(pi.hProcess, 259); }
            CloseHandle(pi.hThread);
            CloseHandle(pi.hProcess);
            return pi.dwProcessId;
        } finally {
            if (env != IntPtr.Zero) { DestroyEnvironmentBlock(env); }
            CloseHandle(primary);
        }
    }

    // Start and DO NOT wait. The hang itself is the subject now, so the process
    // has to stay alive and reachable while it is being looked at; every
    // earlier variant blocked inside the call and could only report a number.
    public static int LaunchDetached(IntPtr token, string cmdLine, string dir, uint logonFlags,
                                     uint creationFlags, bool userEnv, out IntPtr hProcess, out int lastError) {
        hProcess = IntPtr.Zero; lastError = 0;
        IntPtr primary;
        if (!DuplicateTokenEx(token, DupAccess, IntPtr.Zero, 2, 1, out primary)) { lastError = Marshal.GetLastWin32Error(); return -1; }
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
        if (!DuplicateTokenEx(token, DupAccess, IntPtr.Zero, 2, 1, out primary)) { lastError = Marshal.GetLastWin32Error(); return -1; }
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

                # Round 3 granted all three of these, verified them present on
                # WinSta0 and Default, and whoami.exe STILL exited 0xC0000142.
                # So this round stops trying to fix the window station and
                # tries to FALSIFY it instead: Everyone (S-1-1-0) full access.
                # If a child still cannot start with that on both objects, the
                # window station is not what is refusing it and the next round
                # should look somewhere else entirely.
                $grantSids = @($sid, $logonSid, $authSid, 'S-1-1-0') | Where-Object { $_ } | Select-Object -Unique
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

                    # ------------------------------------------------------
                    # Round 8. Seven rounds have varied flags, DACLs, SIDs,
                    # profiles, mechanisms and sessions. The loader has now
                    # named the failure -- USER32's DLL_PROCESS_ATTACH -- and
                    # three separate explanations for it have been falsified.
                    #
                    # What has NEVER been varied is the integrity level. A
                    # UAC-filtered administrator is MEDIUM. This runner logs on
                    # as RID 500 with Admin Approval Mode off, so its own
                    # processes are HIGH, and a window station and desktop
                    # carry a mandatory label that is checked BEFORE the DACL --
                    # which is exactly the shape of "Everyone changed nothing".
                    #
                    # Two controls settle it without any more theory:
                    #   C1 the SAME call with this job's OWN token. If whoami
                    #      fails there too, none of this is about the filtered
                    #      admin and seven rounds have been chasing the wrong
                    #      thing entirely.
                    #   C2 the LINKED token -- same user, same logon session,
                    #      same everything, differing only in elevation.
                    # ------------------------------------------------------
                    $ie = 0
                    $ownTok = [UacProbe]::OwnToken([ref]$ie)
                    foreach ($pair in @(
                        @{ N = "this job (runneradmin)"; T = $ownTok },
                        @{ N = "waired-uacprobe FILTERED"; T = $tok },
                        @{ N = "waired-uacprobe LINKED"; T = $lnk })) {
                        if ($pair.T -eq [IntPtr]::Zero) { Say "  integrity of $($pair.N) = (no token)"; continue }
                        $ie = 0
                        $il = [UacProbe]::IntegrityOf($pair.T, [ref]$ie)
                        $name = switch ($il) {
                            'S-1-16-4096'  { 'Low' }    'S-1-16-8192'  { 'Medium' }
                            'S-1-16-12288' { 'High' }   'S-1-16-16384' { 'System' }
                            default        { '?' }
                        }
                        Say "  integrity of $($pair.N) = $il ($name)"
                    }

                    $c1 = Try-Child -Label 'C1 whoami.exe with THIS JOB''s own token' -Token $ownTok `
                            -Cmd 'C:\Windows\System32\whoami.exe' -CreationFlags 0x08000000 -WaitMs 30000 -ExpectExit 0
                    Say "  C1 (control, our own token): $(if ($c1) { 'RUNS' } else { 'ALSO 0xC0000142 -- the filtered token is not the variable' })"

                    if ($lnk -ne [IntPtr]::Zero) {
                        $c2 = Try-Child -Label 'C2 whoami.exe with the LINKED (full) token' -Token $lnk `
                                -Cmd 'C:\Windows\System32\whoami.exe' -CreationFlags 0x08000000 -WaitMs 30000 -UserEnv $true -ExpectExit 0
                        Say "  C2 (same user, elevated): $(if ($c2) { 'RUNS -- INTEGRITY is the discriminator' } else { 'also refused -- integrity is not it either' })"
                    } else {
                        Say '  C2 skipped: no linked token'
                    }

                    # ------------------------------------------------------
                    # C1 failed. whoami.exe launched through
                    # CreateProcessWithTokenW with THIS JOB'S OWN token -- the
                    # identity the job already runs as, High integrity, the
                    # owner of the desktop -- also dies with 0xC0000142.
                    #
                    # So none of this is about the filtered administrator, the
                    # second user, the logon session, UAC or integrity. Eight
                    # rounds narrowed a property of the TOKEN when the variable
                    # is the CALL. Whatever is wrong is wrong for everyone.
                    #
                    # One field has been set the same way since the first round
                    # and never questioned: STARTUPINFO.lpDesktop. It was set to
                    # "winsta0\default" on the folklore that a child for another
                    # user needs it named. NULL means "inherit the caller's",
                    # which is what an ordinary CreateProcess does -- and an
                    # ordinary CreateProcess is the one thing that works here.
                    # Vary only that, on both tokens.
                    # ------------------------------------------------------
                    $deskWinner = $null
                    foreach ($d in @($null, "$winstaName\$deskName", 'winsta0\default', '')) {
                        foreach ($tk in @(@{ N = 'own'; T = $ownTok }, @{ N = 'filtered'; T = $tok })) {
                            if ($tk.T -eq [IntPtr]::Zero) { continue }
                            [UacProbe]::Desktop = $d
                            $shown = if ($null -eq $d) { 'NULL (inherit)' } elseif ($d -eq '') { "'' (empty)" } else { "'$d'" }
                            $ok = Try-Child -Label "W lpDesktop=$shown token=$($tk.N)" -Token $tk.T `
                                    -Cmd 'C:\Windows\System32\whoami.exe' -CreationFlags 0x08000000 -WaitMs 20000 -ExpectExit 0
                            if ($ok -and $tk.N -eq 'filtered' -and $null -eq $deskWinner) { $deskWinner = @{ D = $d; S = $shown } }
                        }
                    }
                    # Report BOTH tokens. The first version of this line only
                    # considered the filtered one and printed "no lpDesktop
                    # value works for either token" over data showing the own
                    # token exiting 0 twice.
                    if ($deskWinner) {
                        [UacProbe]::Desktop = $deskWinner.D
                        Say "  *** lpDesktop = $($deskWinner.S) lets the FILTERED admin start a user32 process ***"
                    } else {
                        # Measured, run 32593263826: NULL and '' both run for the
                        # OWN token; every explicit name fails for BOTH. So
                        # naming the desktop is a defect in its own right, and
                        # inheriting is the correct setting from here on.
                        [UacProbe]::Desktop = $null
                        Say '  no lpDesktop value lets the FILTERED token start a user32 process.'
                        Say '  But NULL/empty DO work for our own token and every explicit name fails for both,'
                        Say '  so naming the desktop was itself broken. Inheriting from here on.'
                    }

                    # ------------------------------------------------------
                    # R -- CreateProcessWithLogonW, the API `runas` uses.
                    # Everything tried so far assembles by hand what this does
                    # internally: LogonUser, then a process created through
                    # seclogon onto a window station and desktop this probe had
                    # to grant itself, with a profile it had to load itself.
                    # Ten rounds of negative results are all about that hand
                    # assembly. This is the control none of them had, it needs
                    # no privilege, and the password is ours because the probe
                    # created the account.
                    # ------------------------------------------------------
                    [UacProbe]::Desktop = $null
                    function Try-Logon {
                        param([string]$Label, [string]$Cmd, [uint32]$LogonFlags, [string]$Marker = '',
                              [int]$WaitMs = 60000, [int]$ExpectExit = -12345)
                        if ($Marker) { Remove-Item -LiteralPath $Marker -ErrorAction SilentlyContinue }
                        $ec = -1; $wr = -1; $er = 0
                        $pp = [UacProbe]::LaunchWithLogonAndWait($u, '.', $pw, $LogonFlags, $Cmd, $pub, $WaitMs, 0x08000000,
                                    [ref]$ec, [ref]$wr, [ref]$er)
                        if ($pp -lt 0) { Say "  ${Label}: CreateProcessWithLogonW FAILED lastError=$er"; return $false }
                        $note = if ($ec -eq -1073741502) { '  <- STATUS_DLL_INIT_FAILED' } elseif ($ec -eq 259) { '  <- STILL_ACTIVE' } else { '' }
                        Say ("  {0}: pid={1} exit={2} (0x{2:X8}){3}" -f $Label, $pp, $ec, $note)
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
                    # $null, not $false, as the sentinel: the winning value can
                    # BE 0, and `0 -ne $false` is False in PowerShell, so a
                    # success with logonFlags=0 would have read as a failure.
                    $rWhoami = $null
                    foreach ($lf in @(0, 1)) {   # 1 = LOGON_WITH_PROFILE
                        $ok = Try-Logon -Label "R whoami.exe via CreateProcessWithLogonW (logonFlags=$lf)" `
                                -Cmd 'C:\Windows\System32\whoami.exe' -LogonFlags $lf -WaitMs 60000 -ExpectExit 0
                        if ($ok) { $rWhoami = $lf; break }
                    }
                    if ($null -ne $rWhoami) {
                        Say "  *** CreateProcessWithLogonW RUNS a user32 process as the second user (logonFlags=$rWhoami) ***"
                        $rDone = Join-Path $pub 'r-ps.done'
                        $rPs = Try-Logon -Label 'R powershell 5.1 via CreateProcessWithLogonW' -LogonFlags $rWhoami `
                                -Cmd "$PS51 -NoProfile -NonInteractive -Command `"Set-Content -LiteralPath '$rDone' -Value ok; exit 7`"" `
                                -Marker $rDone -WaitMs 120000 -ExpectExit 7
                        if ($rPs) {
                            Say '  powershell RUNS as the filtered admin -- asking for the elevation'
                            [void](Try-Logon -Label 'R child2.ps1 -> Start-Process -Verb RunAs' -LogonFlags $rWhoami `
                                    -Cmd "$PS51 -NoProfile -NonInteractive -ExecutionPolicy Bypass -File `"$childPs2`"" `
                                    -Marker $rep -WaitMs 150000)
                            foreach ($f in @($rep, $mark)) {
                                if (Test-Path -LiteralPath $f) {
                                    foreach ($l in (Get-Content -LiteralPath $f)) { Say "    $(Split-Path $f -Leaf): $l" }
                                } else { Say "    $(Split-Path $f -Leaf): absent" }
                            }
                            if (Test-Path -LiteralPath $mark) { Say '  *** a GRANTED UAC ELEVATION ran with no human present ***' }
                        }
                    } else {
                        Say '  CreateProcessWithLogonW cannot start a user32 process either'
                    }

                    # ------------------------------------------------------
                    # With the desktop inherited, the confound is gone and the
                    # experiments rounds 3-4 MEANT to run can finally run.
                    # ------------------------------------------------------
                    [UacProbe]::Desktop = $null
                    Say '  lpDesktop is NULL (inherit) from here -- measured correct in run 32593263826'

                    # The mandatory label of the objects the child must attach
                    # to. Checked before the DACL, so it can refuse a Medium
                    # child while Everyone sits in the DACL doing nothing.
                    foreach ($pair in @(@{ N = 'window station'; H = $hWinsta }, @{ N = 'desktop'; H = $hDesk })) {
                        $le = 0
                        $lab = [UacProbe]::GetLabel($pair.H, [ref]$le)
                        if (-not $lab) { Say "  mandatory label of the $($pair.N): unreadable (lastError=$le)"; continue }
                        $lsd = New-Object Security.AccessControl.RawSecurityDescriptor($lab, 0)
                        if ($null -eq $lsd.SystemAcl -or $lsd.SystemAcl.Count -eq 0) {
                            Say "  mandatory label of the $($pair.N): none (defaults to Medium)"
                            continue
                        }
                        foreach ($ace in $lsd.SystemAcl) {
                            $bytes = New-Object byte[] $ace.BinaryLength
                            $ace.GetBinaryForm($bytes, 0)
                            # SYSTEM_MANDATORY_LABEL_ACE: 4-byte header, 4-byte mask, then the SID.
                            $lsid = try { (New-Object Security.Principal.SecurityIdentifier($bytes, 8)).Value } catch { '(unparsed)' }
                            $lname = switch ($lsid) {
                                'S-1-16-4096'  { 'Low' }    'S-1-16-8192'  { 'Medium' }
                                'S-1-16-12288' { 'High' }   'S-1-16-16384' { 'System' }
                                default        { '?' }
                            }
                            Say "  mandatory label of the $($pair.N): aceType=$($ace.AceType) $lsid ($lname)"
                        }
                    }

                    # C2 again, with the rights CreateProcessWithTokenW actually
                    # documents. Same user, same logon session, same inherited
                    # desktop -- differing ONLY in elevation. If this runs and
                    # the filtered one does not, it is integrity.
                    if ($lnk -ne [IntPtr]::Zero) {
                        [UacProbe]::DupAccess = [UacProbe]::TOKEN_RIGHTS_FOR_CREATEPROCESS
                        $c2b = Try-Child -Label 'C2b whoami.exe, LINKED (full) token, inherited desktop' -Token $lnk `
                                -Cmd 'C:\Windows\System32\whoami.exe' -CreationFlags 0x08000000 -WaitMs 30000 -ExpectExit 0
                        [UacProbe]::DupAccess = [UacProbe]::MAXIMUM_ALLOWED
                        Say "  C2b (same user, elevated): $(if ($c2b) { 'RUNS -- INTEGRITY is the discriminator' } else { 'still refused' })"
                    }

                    # E1 -- the cheapest possible indicator. whoami.exe loads
                    # user32 and nothing else interesting, so it answers the
                    # question on its own and in a second.
                    $e1 = Try-Child -Label 'E1 whoami.exe (CreateProcessWithTokenW)' -Token $tok -Cmd 'C:\Windows\System32\whoami.exe' `
                            -CreationFlags 0x08000000 -WaitMs 30000 -ExpectExit 0
                    Say "  E1 via seclogon: $(if ($e1) { 'RUNS' } else { 'still 0xC0000142' })"

                    # E7 -- LOAD THE PROFILE. Every round so far ran in a logon
                    # session with no profile at all: C:\Users\<user> never
                    # appeared and there is no HKCU hive. That is the one thing
                    # common to all of them, and unlike the earlier
                    # LOGON_WITH_PROFILE attempts this is done HERE, where its
                    # success or failure is visible, with the two things it
                    # actually creates checked afterwards.
                    foreach ($pv in @('SeRestorePrivilege','SeBackupPrivilege')) {
                        $pe = 0
                        Say "  $pv enabled = $([UacProbe]::EnablePrivilege($pv, [ref]$pe))$(if ($pe) { " (lastError=$pe)" })"
                    }
                    $pe = 0
                    $hProfile = [UacProbe]::LoadProfile($tok, $u, [ref]$pe)
                    if ($hProfile -eq [IntPtr]::Zero) {
                        Say "  E7 LoadUserProfile FAILED (lastError=$pe)"
                    } else {
                        Say '  E7 LoadUserProfile returned a handle'
                    }
                    Say "    profile dir now exists = $(Test-Path -LiteralPath "C:\Users\$u")"
                    Say "    HKU\$sid hive loaded  = $(Test-Path -LiteralPath "Registry::HKEY_USERS\$sid")"
                    $e7 = Try-Child -Label 'E7 whoami.exe (profile loaded)' -Token $tok -Cmd 'C:\Windows\System32\whoami.exe' `
                            -CreationFlags 0x08000000 -WaitMs 30000 -UserEnv $true -ExpectExit 0
                    Say "  E7 with a loaded profile: $(if ($e7) { 'RUNS -- the missing profile was it' } else { 'still 0xC0000142' })"

                    # E8 -- if it still fails, stop reasoning about it and let
                    # the loader say which DLL it is. Loader snaps print a line
                    # per DLL init; the one that returns FALSE is the answer.
                    # The child is created SUSPENDED so a debugger can turn the
                    # snaps on before any DllMain has run.
                    if (-not $e7) {
                        $cdb = @('C:\Program Files (x86)\Windows Kits\10\Debuggers\x64\cdb.exe',
                                 'C:\Program Files\Windows Kits\10\Debuggers\x64\cdb.exe') |
                               Where-Object { Test-Path -LiteralPath $_ } | Select-Object -First 1
                        if (-not $cdb) {
                            Say '  E8 skipped: no cdb.exe on this image'
                        } else {
                            $hS = [IntPtr]::Zero; $eS = 0
                            $pidS = [UacProbe]::LaunchDetached($tok, 'C:\Windows\System32\whoami.exe', $pub,
                                        0, (0x08000000 -bor 0x00000004), $true, [ref]$hS, [ref]$eS)   # CREATE_SUSPENDED
                            if ($pidS -lt 0) {
                                Say "  E8: could not create the suspended child (lastError=$eS)"
                            } else {
                                Say "  E8: suspended whoami pid=$pidS ; attaching cdb with loader snaps on"
                                $job = Start-Job -ScriptBlock {
                                    param($exe, $procId)
                                    # !gflag +sls turns on FLG_SHOW_LDR_SNAPS in the
                                    # debuggee; ~0m releases the CREATE_SUSPENDED
                                    # count so the loader finally runs, under
                                    # observation. -G quits when it exits.
                                    # Each `g` in the -c list runs at the NEXT break.
                                    # One was not enough: the loader takes its own
                                    # debugger break once a debugger is present, and
                                    # cdb sat at that prompt with nothing queued, so
                                    # the snaps stopped just before the init routines.
                                    & $exe -p $procId -G -c '!gflag +sls; ~0m; g; g; g; g; q' 2>&1 | Out-String
                                } -ArgumentList $cdb, $pidS
                                if (Wait-Job $job -Timeout 180) {
                                    $out = (Receive-Job $job) -split "`r?`n"
                                    $ldr = @($out | Where-Object { $_ -match ' - ERROR:| - WARNING:|DllMain|returned FALSE|INIT_FAILED|Unable to load|LdrpInitializeNode|LdrpCallInitRoutine|LdrpProcessWork.*fail' })
                                    Say "  E8 loader snaps: $($ldr.Count) relevant line(s)"
                                    foreach ($l in ($ldr | Select-Object -Last 80)) { Say "    ldr $l" }
                                    Say '  E8 last lines of the session regardless of filter:'
                                    foreach ($l in ($out | Where-Object { $_ -match '\S' } | Select-Object -Last 25)) { Say "    end $l" }
                                    if (-not $ldr.Count) {
                                        foreach ($l in ($out | Where-Object { $_ -match '\S' } | Select-Object -Last 40)) { Say "    cdb $l" }
                                    }
                                } else {
                                    Say '  E8: cdb did not finish in 180s'
                                    Stop-Job $job -ErrorAction SilentlyContinue
                                }
                                Remove-Job $job -Force -ErrorAction SilentlyContinue
                                [void][UacProbe]::TerminateProcess($hS, 1)
                                [void][UacProbe]::CloseHandle($hS)
                            }
                        }
                    }

                    # E6 -- the same child through CreateProcessAsUser instead.
                    # Round 4 ruled out the window station (Everyone had full
                    # access to WinSta0 and Default and it changed nothing), and
                    # the surviving difference between a child that runs and one
                    # that does not is user32. The remaining untested variable is
                    # not a flag but the MECHANISM: everything so far has been
                    # created by the Secondary Logon service over RPC.
                    foreach ($pv in @('SeAssignPrimaryTokenPrivilege','SeIncreaseQuotaPrivilege','SeTcbPrivilege')) {
                        $pe = 0
                        Say "  $pv enabled = $([UacProbe]::EnablePrivilege($pv, [ref]$pe))$(if ($pe) { " (lastError=$pe)" })"
                    }
                    function Try-AsUser {
                        param([string]$Label, [string]$Cmd, [string]$Marker = '', [int]$WaitMs = 30000,
                              [bool]$UserEnv = $true, [int]$ExpectExit = -12345)
                        if ($Marker) { Remove-Item -LiteralPath $Marker -ErrorAction SilentlyContinue }
                        $ec = -1; $wr = -1; $er = 0
                        $pp = [UacProbe]::LaunchAsUserAndWait($tok, $Cmd, $pub, $WaitMs, 0x08000000, $UserEnv, [ref]$ec, [ref]$wr, [ref]$er)
                        if ($pp -lt 0) {
                            Say "  ${Label}: CreateProcessAsUser FAILED lastError=$er (1314 = SeAssignPrimaryToken not held/enabled)"
                            return $false
                        }
                        $note = if ($ec -eq -1073741502) { '  <- STATUS_DLL_INIT_FAILED' } elseif ($ec -eq 259) { '  <- STILL_ACTIVE' } else { '' }
                        Say ("  {0}: pid={1} exit={2} (0x{2:X8}){3}" -f $Label, $pp, $ec, $note)
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
                    # Measured: an interactive administrator does NOT hold
                    # SeAssignPrimaryTokenPrivilege (it is granted to LOCAL
                    # SERVICE and NETWORK SERVICE by default), so this returns
                    # 1314 and the mechanism is unavailable without a SYSTEM
                    # helper. Kept because that is the record.
                    $e6 = Try-AsUser -Label 'E6 whoami.exe (CreateProcessAsUser)' -Cmd 'C:\Windows\System32\whoami.exe' -ExpectExit 0
                    Say "  E6 via CreateProcessAsUser: $(if ($e6) { 'RUNS -- the mechanism was the difference' } else { 'refused (see the privilege above)' })"

                    $ps51Done = Join-Path $pub 'ps51.done'
                    $psCmd = "$PS51 -NoProfile -NonInteractive -Command `"Set-Content -LiteralPath '$ps51Done' -Value ok; exit 7`""
                    if ($e6) {
                        $e3 = Try-AsUser -Label 'E3 powershell 5.1 (CreateProcessAsUser)' -Cmd $psCmd `
                                -Marker $ps51Done -WaitMs 90000 -ExpectExit 7
                        $useAsUser = $true
                    } else {
                        $e3 = Try-Child -Label 'E3 powershell 5.1 (seclogon)' -Token $tok -Cmd $psCmd `
                                -Marker $ps51Done -CreationFlags 0x08000000 -WaitMs 90000 -UserEnv $true -ExpectExit 7
                        $useAsUser = $false
                    }

                    if (-not $e3) {
                        Say '  powershell still does not start; the payoff below is not attempted'
                        if (-not $e1) {
                            # RETRACTED. Round 4 concluded from this that the
                            # window station was exonerated. That test ran with
                            # lpDesktop pinned to "winsta0\default", which round
                            # 9 measured as broken on its own -- so it never
                            # tested what it claimed. The grant is only
                            # meaningful now that the desktop is inherited.
                            Say '  => Everyone has full access to WinSta0 and Default and the child still cannot start.'
                            Say '     (Round 4 read this as exonerating the window station. That reading was made with'
                            Say '      lpDesktop pinned to a value round 9 showed is broken by itself, so it is retracted.)'
                        }

                        # Which DLL is failing its init? The loader loads and
                        # initialises in order, so the difference between a
                        # child that RUNS with this token and one that does not
                        # names the suspect. cmd.exe is the only known survivor,
                        # so it is the control.
                        $e = 0
                        if ([UacProbe]::EnablePrivilege('SeDebugPrivilege', [ref]$e)) { Say '  SeDebugPrivilege enabled for the module diff' }
                        else { Say "  SeDebugPrivilege NOT enabled (lastError=$e); the diff may come back empty" }

                        function Dump-Modules {
                            param([string]$Label, [int]$ProcId)
                            $pp = Get-Process -Id $ProcId -ErrorAction SilentlyContinue
                            if (-not $pp) { Say "    ${Label}: already gone"; return @() }
                            $ml = $pp.Modules
                            if ($null -eq $ml) { Say "    ${Label}: modules unreadable"; return @() }
                            $names = @($ml | Where-Object { $_ } | ForEach-Object { $_.ModuleName })
                            Say "    ${Label}: $($names.Count) modules in load order:"
                            Say "      $($names -join ' ')"
                            return $names
                        }

                        # The failing one has to stay alive to be looked at, and
                        # it only stays alive while hard errors are NOT
                        # suppressed -- the blocked dialog is what holds it open.
                        Set-ItemProperty -LiteralPath $emKey -Name ErrorMode -Value 0 -Type DWord -ErrorAction SilentlyContinue
                        $hA = [IntPtr]::Zero; $eA = 0
                        $pidFail = [UacProbe]::LaunchDetached($tok, 'C:\Windows\System32\whoami.exe', $pub, 0, 0x08000000, $true, [ref]$hA, [ref]$eA)
                        $hB = [IntPtr]::Zero; $eB = 0
                        $pidOk = [UacProbe]::LaunchDetached($tok, 'cmd.exe /c ping -n 30 127.0.0.1', $pub, 0, 0x08000000, $true, [ref]$hB, [ref]$eB)
                        Say "  module diff: failing pid=$pidFail (whoami) vs working pid=$pidOk (cmd)"
                        function Session-Of {
                            param([string]$Label, [int]$ProcId)
                            $ci = Get-CimInstance Win32_Process -Filter "ProcessId=$ProcId" -ErrorAction SilentlyContinue
                            if ($ci) { Say "    ${Label}: pid=$ProcId sessionId=$($ci.SessionId)" }
                            else { Say "    ${Label}: pid=$ProcId already gone" }
                        }
                        Session-Of -Label 'the probe itself' -ProcId $PID
                        Session-Of -Label 'whoami (fails)  ' -ProcId $pidFail
                        Session-Of -Label 'cmd    (runs)   ' -ProcId $pidOk
                        foreach ($svc in @('seclogon')) {
                            $sp = Get-CimInstance Win32_Service -Filter "Name='$svc'" -ErrorAction SilentlyContinue
                            if ($sp -and $sp.ProcessId) { Session-Of -Label "$svc service   " -ProcId $sp.ProcessId }
                            else { Say "    ${svc}: not running" }
                        }
                        Start-Sleep -Seconds 6
                        $modFail = Dump-Modules -Label 'whoami (fails)' -ProcId $pidFail
                        $modOk   = Dump-Modules -Label 'cmd    (runs)'  -ProcId $pidOk
                        if ($modFail.Count -and $modOk.Count) {
                            $only = @($modFail | Where-Object { $modOk -notcontains $_ })
                            Say "    only in the FAILING one: $(if ($only.Count) { $only -join ' ' } else { '(nothing)' })"
                            Say "    last module the failing one loaded: $($modFail[-1])"
                        }
                        foreach ($h in @($hA, $hB)) {
                            if ($h -ne [IntPtr]::Zero) { [void][UacProbe]::TerminateProcess($h, 1); [void][UacProbe]::CloseHandle($h) }
                        }
                        Set-ItemProperty -LiteralPath $emKey -Name ErrorMode -Value 2 -Type DWord -ErrorAction SilentlyContinue

                        # The System log is where a suppressed hard error is
                        # recorded. Earlier rounds only read Application, which
                        # is why they came back empty.
                        foreach ($log in @('System', 'Application')) {
                            try {
                                $evs = @(Get-WinEvent -FilterHashtable @{ LogName = $log; StartTime = $probeStart } -ErrorAction Stop |
                                         Where-Object { $_.LevelDisplayName -ne 'Information' } | Select-Object -First 10)
                                Say "  ${log} log: $($evs.Count) non-informational event(s)"
                                foreach ($ev in $evs) { Say "    [$($ev.LevelDisplayName)] $($ev.Id) $(($ev.Message -split "`r?`n" | Select-Object -First 1))" }
                            } catch { Say "  ${log} log: none" }
                        }
                    } else {
                        # THE PAYOFF. A filtered admin that can run PowerShell
                        # can be asked the one question waired-agent#997 says
                        # is never executed: does Start-Process -Verb RunAs
                        # get a GRANTED elevation without a human?
                        Say '  powershell 5.1 RUNS as the filtered admin -- asking for the elevation'
                        $runCmd = "$PS51 -NoProfile -NonInteractive -ExecutionPolicy Bypass -File `"$childPs2`""
                        if ($useAsUser) {
                            [void](Try-AsUser -Label 'E5 child2.ps1 -> Start-Process -Verb RunAs' -Cmd $runCmd -Marker $rep -WaitMs 120000)
                        } else {
                            [void](Try-Child -Label 'E5 child2.ps1 -> Start-Process -Verb RunAs' -Token $tok -Cmd $runCmd `
                                    -Marker $rep -CreationFlags 0x08000000 -WaitMs 120000 -UserEnv $true)
                        }
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
                    if ($hProfile -and $hProfile -ne [IntPtr]::Zero) {
                        # A loaded profile keeps the account in use and makes
                        # Remove-LocalUser fail with an unrelated-looking error.
                        Say "  profile unloaded = $([UacProbe]::UnloadUserProfile($tok, $hProfile))"
                    }
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

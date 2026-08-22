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
                                    uint creationFlags, out int exitCode, out int waitResult, out int lastError) {
        exitCode = -1; waitResult = -1; lastError = 0;
        IntPtr primary;
        if (!DuplicateTokenEx(token, MAXIMUM_ALLOWED, IntPtr.Zero, 2, 1, out primary)) { lastError = Marshal.GetLastWin32Error(); return -1; }
        try {
            STARTUPINFO si = new STARTUPINFO();
            si.cb = Marshal.SizeOf(si);
            si.lpDesktop = "winsta0\\default";
            PROCESS_INFORMATION pi;
            if (!CreateProcessWithTokenW(primary, logonFlags, null, cmdLine, creationFlags, IntPtr.Zero, dir, ref si, out pi)) {
                lastError = Marshal.GetLastWin32Error(); return -1;
            }
            waitResult = WaitForSingleObject(pi.hProcess, waitMs);
            GetExitCodeProcess(pi.hProcess, out exitCode);
            CloseHandle(pi.hThread);
            CloseHandle(pi.hProcess);
            return pi.dwProcessId;
        } finally { CloseHandle(primary); }
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
                function Try-Child {
                    param([string]$Label, [IntPtr]$Token, [string]$Cmd, [string]$Marker,
                          [uint32]$LogonFlags = 0, [uint32]$CreationFlags = 0, [int]$WaitMs = 20000)
                    Remove-Item -LiteralPath $Marker -ErrorAction SilentlyContinue
                    $ec = -1; $wr = -1; $e = 0
                    $p = [UacProbe]::LaunchAndWait($Token, $Cmd, $pub, $WaitMs, $LogonFlags, $CreationFlags, [ref]$ec, [ref]$wr, [ref]$e)
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
                    if (Test-Path -LiteralPath $Marker) {
                        foreach ($l in Get-Content -LiteralPath $Marker) { Say "    ${Label} said: $l" }
                        return $true
                    }
                    Say "    ${Label}: wrote nothing"
                    return $false
                }

                # Open the window station and desktop to the new user FIRST.
                # The previous round ran the child before this and then threw
                # inside the DACL step, so the "no ACL" result was the only one
                # ever measured -- ordering the cheap prerequisite first avoids
                # spending a whole run on a case nobody wants.
                #
                # AceFlags is a [Flags] enum but PowerShell would not take -bor
                # on it ("does not contain a method named 'op_BitwiseOr'"), so
                # the flags are parsed from their string form instead. Measured.
                $granted = $false
                try {
                    $usid  = New-Object Security.Principal.SecurityIdentifier($sid)
                    $flags = [Security.AccessControl.AceFlags]'ObjectInherit, ContainerInherit'
                    foreach ($pair in @(
                        @{ Name = 'window station'; Handle = [UacProbe]::GetProcessWindowStation(); Mask = 0x0000037F },
                        @{ Name = 'desktop';        Handle = [UacProbe]::GetThreadDesktop([UacProbe]::GetCurrentThreadId()); Mask = 0x000001FF }
                    )) {
                        $e = 0
                        $raw = [UacProbe]::GetDacl($pair.Handle, [ref]$e)
                        if (-not $raw) { Say "  could not read the $($pair.Name) DACL (lastError=$e)"; continue }
                        $rsd = New-Object Security.AccessControl.RawSecurityDescriptor($raw, 0)
                        $ace = New-Object Security.AccessControl.CommonAce(
                            $flags, [Security.AccessControl.AceQualifier]::AccessAllowed, $pair.Mask, $usid, $false, $null)
                        $rsd.DiscretionaryAcl.InsertAce(0, $ace)
                        $buf = New-Object byte[] $rsd.BinaryLength
                        $rsd.GetBinaryForm($buf, 0)
                        $e = 0
                        if ([UacProbe]::SetDacl($pair.Handle, $buf, [ref]$e)) { Say "  granted $($pair.Name) access to $u"; $granted = $true }
                        else { Say "  could not set the $($pair.Name) DACL (lastError=$e)" }
                    }
                } catch { Say "  DACL step threw: $($_.Exception.Message)" }
                Say "  DACLs granted = $granted"

                # `echo`, not `whoami`: one builtin, no separate image to load,
                # so a failure here is about the process starting and nothing
                # else. LOGON_WITH_PROFILE (0x1) is the second axis -- the new
                # user has no loaded profile, and that is a plausible cause of a
                # hang rather than a crash.
                # The ACLs are in place and a one-builtin child still hangs, so
                # the remaining suspect is the CONSOLE: with dwCreationFlags=0
                # a console child tries to attach to the caller's console, and
                # the caller is a different user in a different logon session.
                # Four variants in one run rather than four dispatches.
                $cmdEcho  = "cmd.exe /c echo alive> `"$alive`""
                $variants = @(
                    @{ Label = 'A dwCreationFlags=0';        C = 0x00000000; L = 0 },
                    @{ Label = 'B CREATE_NEW_CONSOLE';       C = 0x00000010; L = 0 },
                    @{ Label = 'C CREATE_NO_WINDOW';         C = 0x08000000; L = 0 },
                    @{ Label = 'D DETACHED_PROCESS';         C = 0x00000008; L = 0 },
                    @{ Label = 'E NEW_CONSOLE + WITH_PROFILE'; C = 0x00000010; L = 1 }
                )
                $winner = $null
                foreach ($v in $variants) {
                    if (Try-Child -Label $v.Label -Token $tok -Cmd $cmdEcho -Marker $alive -LogonFlags $v.L -CreationFlags $v.C -WaitMs 15000) {
                        $winner = $v
                        break
                    }
                }

                if (-not $winner) {
                    Say '  RunAs skipped: no variant can run a child at all, so a RunAs result would mean nothing'
                } else {
                    Say "  a cmd child CAN run with $($winner.Label)"

                    # cmd runs under CREATE_NO_WINDOW but powershell wrote no
                    # breadcrumb at all -- not even "01 started" -- so it hangs
                    # BEFORE its first statement. That is startup, and the
                    # prime suspect is the profile: the winning cmd variant used
                    # logonFlags=0, so this user has none loaded.
                    #
                    # This matters beyond the probe: the real installer IS
                    # PowerShell (install.ps1), so a route that cannot start
                    # powershell.exe is no route at all.
                    $psVariants = @(
                        @{ Label = 'PS NO_WINDOW, no profile';   C = 0x08000000; L = 0 },
                        @{ Label = 'PS NO_WINDOW + WITH_PROFILE'; C = 0x08000000; L = 1 },
                        @{ Label = 'PS DETACHED + WITH_PROFILE';  C = 0x00000008; L = 1 }
                    )
                    $psWin = $null
                    foreach ($v in $psVariants) {
                        # First start on a fresh profile is slow; give it room,
                        # but bound it -- a hang must not become a 28-minute run.
                        [void](Try-Child -Label $v.Label -Token $tok `
                            -Cmd "powershell.exe -NoProfile -ExecutionPolicy Bypass -File `"$childPs2`"" `
                            -Marker $rep -LogonFlags $v.L -CreationFlags $v.C -WaitMs 90000)
                        if (Test-Path -LiteralPath $rep) { $psWin = $v; break }
                    }
                    if (-not $psWin) {
                        Say '  powershell.exe never reached its first statement under any variant'
                    }
                    if (Test-Path -LiteralPath $mark) {
                        foreach ($l in Get-Content -LiteralPath $mark) { Say "    *** GRANTED ELEVATION: $l" }
                    } else {
                        Say '    no elevated grandchild -- see the last breadcrumb above for where it stopped'
                    }
                }
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

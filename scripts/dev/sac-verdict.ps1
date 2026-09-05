#!/usr/bin/env pwsh
<#
.SYNOPSIS
  Read what Windows Code Integrity recorded about our programs: which files an
  application-control policy refused, when, and what Microsoft's reputation
  service answered for each refusal.

.DESCRIPTION
  waired-ai/waired-agent#1191 asks what makes Smart App Control refuse a Waired
  program and what makes the refusal lift. The 0.0.3-rc5 campaign
  (waired-ai/waired#1309) captured only the installer's own wording, so the
  answer was not in the evidence. It is, however, still on the machines: the
  Microsoft-Windows-CodeIntegrity/Operational log keeps the events, it is
  readable WITHOUT elevation, and on a real host it reaches back months.

  Two events carry the answer.

    3077  the refusal itself: file name, the four hashes CodeIntegrity
          reports, requested/validated signing level, policy name and GUID,
          and the PE's own version resource. (3076 is its audit-mode twin,
          3033 the enforcement variant CodeIntegrity also writes.)
    3118  "Smart App Control Block Details" -- the reputation answer for that
          same file: whether Defender was called, whether a cloud call was
          requested and made, the HTTP code that came back, the trust value,
          the cached trust value, and whether the file was judged unfriendly.

  3118 is the half nobody was reading. `internal/platform/servicediag` reads
  3033/3077, `installtest-windows.ps1 -SacAudit` reads 3076, and nothing reads
  3118 -- which is why the reputation verdict looked unobservable without the
  documented `TestFlags=0x300` + reboot that turns on events 3090-3092. That
  registry change is still the only way to see the ISG's answer for a file it
  ALLOWED; for a file it REFUSED, 3118 already says it.

  The field set of 3118 is not the same on every Windows build -- a 25H2 host
  and a 24H2 host answer with different field counts -- so every event is
  parsed generically: whatever <Data Name=...> nodes are present are kept.
  Nothing here assumes a field exists.

.PARAMETER Harvest
  Read what the log already holds and write the table. Needs no install
  attempt, changes nothing, and is the fastest way to fill in the history for
  a host that has been refusing binaries for weeks.

.PARAMETER Attempt
  Run this right after an install/launch attempt, naming the files that were
  tried. Adds the host state that only means something at the time of the
  attempt: each file's SHA256 and extended attributes, the App Control policy
  list, Defender's configuration, and the AppID services.

.PARAMETER Match
  Keep only files whose bucket/name matches this regular expression, e.g.
  'waired'. The host-wide default is the honest one -- a Waired binary refused
  in the same hour a browser's DLL was allowed is the comparison #1191 needs --
  but a table for an issue usually wants one product's rows.

.PARAMETER Since
  Only read events at or after this time (any string DateTime accepts, e.g.
  '2026-08-30' or '2026-08-30T18:00:00Z'). Default: everything in the log.

.PARAMETER OutDir
  Where to write. Default: the current directory. Three files are written:
  <prefix>.json, <prefix>.md, and <prefix>-policies.json (the policy list,
  kept separate because it is long and nobody reads it inline).

.PARAMETER Label
  A short tag folded into the output file names, so several attempts on one
  host do not overwrite each other.

.PARAMETER NoRedact
  Keep user names, the computer name and volume paths as they are. The
  default replaces them, because these tables get pasted into public issues.

.EXAMPLE
  powershell -NoProfile -File scripts\dev\sac-verdict.ps1 -Harvest -OutDir C:\sac

.EXAMPLE
  powershell -NoProfile -File scripts\dev\sac-verdict.ps1 `
      -Attempt 'C:\Program Files\Waired\waired-agent.exe' -Label try7 -OutDir C:\sac
#>
[CmdletBinding(DefaultParameterSetName = 'Harvest')]
param(
    [Parameter(ParameterSetName = 'Harvest')]
    [switch]$Harvest,

    [Parameter(ParameterSetName = 'Attempt', Mandatory = $true)]
    [string[]]$Attempt,

    [string]$Since,
    [string]$Match,
    [string]$OutDir,
    [string]$Label,
    [int]$MaxEvents = 5000,
    [switch]$NoRedact
)

# Deliberately NOT 'Stop': this is a collector. A host that answers half the
# questions must still write the half it answered -- an empty file because
# `fsutil` was missing is the failure mode this exists to avoid
# (docs/knowledges/20260830/0330-a-collector-is-only-as-good-as-what-it-can-reach.md).
$ErrorActionPreference = 'Continue'

$CiLog = 'Microsoft-Windows-CodeIntegrity/Operational'

# 3076 audit / 3077 enforcement blocks / 3033 the enforcement variant
# CodeIntegrity also writes / 3089 signature detail / 3090-3092 the ISG
# diagnostic events that only exist under TestFlags=0x300 / 3118 the Smart App
# Control block details.
$CiEventIds = @(3033, 3076, 3077, 3089, 3090, 3091, 3092, 3118)

$script:HostName = $env:COMPUTERNAME
$script:UserName = $env:USERNAME

function Write-Note([string]$Text) { Write-Host "[sac-verdict] $Text" }

# Redaction. These tables are meant to be pasted into waired-agent issues,
# which are public; the host nicknames already used in #1191 are fine, a
# machine name and a user profile path are not.
function Protect-Text {
    param([string]$Text)
    if ($null -eq $Text) { return $null }
    if ($NoRedact) { return $Text }
    $out = $Text
    $out = [regex]::Replace($out, '\\Device\\HarddiskVolume\d+', '<volume>')
    $out = [regex]::Replace($out, '\\\?\?\\[A-Za-z]:', '<volume>')
    if ($script:UserName) {
        $out = [regex]::Replace($out, [regex]::Escape($script:UserName), '<user>', 'IgnoreCase')
    }
    if ($script:HostName) {
        $out = [regex]::Replace($out, [regex]::Escape($script:HostName), '<host>', 'IgnoreCase')
    }
    return $out
}

# Same bucketing rule as Get-SacInventoryKey in installtest-windows.ps1, so the
# two tables can be read side by side. Windows is matched before Temp on
# purpose: %WINDIR%\Temp is Windows, not a staging directory of ours.
function Get-FileKey {
    param([string]$NtPath)
    if (-not $NtPath) { return '' }
    $p = $NtPath -replace '^\\Device\\HarddiskVolume\d+', ''
    $p = $p -replace '^\\\?\?\\[A-Za-z]:', ''
    $name = Split-Path -Leaf $p
    $bucket = 'Other'
    if     ($p -match '(?i)\\Program Files\\') { $bucket = 'ProgramFiles' }
    elseif ($p -match '(?i)\\ProgramData\\')   { $bucket = 'ProgramData' }
    elseif ($p -match '(?i)\\Windows\\')       { $bucket = 'Windows' }
    elseif ($p -match '(?i)\\(Temp|TMP)\\')    { $bucket = 'Temp' }
    elseif ($p -match '(?i)\\Users\\')         { $bucket = 'Users' }
    return "$bucket/$name"
}

# Every <Data Name="..."> node, whatever they happen to be on this build.
#
# $Record, not $Event: $Event is a PowerShell automatic variable (the one an
# event action's script block is handed), and a parameter of that name shadows
# it inside this function. Harmless here, but it is the same shape as the
# -Args parameter that shadowed $args in installtest-pwsh.ps1 -- fixed rather
# than excluded when this lint was introduced, and again now (waired-agent#1224).
function ConvertTo-EventData {
    param($Record)
    $d = [ordered]@{}
    try {
        $xml = [xml]$Record.ToXml()
        foreach ($n in $xml.Event.EventData.Data) {
            if ($n.Name) { $d[[string]$n.Name] = [string]$n.'#text' }
        }
    } catch {
        $d['ParseError'] = $_.Exception.Message
    }
    return $d
}

function Get-Field {
    param($Data, [string[]]$Names)
    foreach ($n in $Names) {
        if ($Data.Contains($n)) {
            $v = $Data[$n]
            if ($null -ne $v -and $v -ne '') { return $v }
        }
    }
    return $null
}

function Get-CiEvents {
    # Untyped on purpose: a [datetime] parameter cannot be bound to $null, and
    # "no -Since given" is the common case.
    param($StartTime)
    $filter = @{ LogName = $CiLog; Id = $CiEventIds }
    if ($StartTime) { $filter['StartTime'] = [datetime]$StartTime }
    $ev = @()
    try {
        $ev = @(Get-WinEvent -FilterHashtable $filter -MaxEvents $MaxEvents -ErrorAction Stop)
    } catch {
        Write-Note "no matching events, or the log could not be read: $($_.Exception.Message)"
    }
    return $ev
}

# Windows PowerShell's -Encoding UTF8 writes a byte-order mark, which shows up
# as a stray character at the top of a Markdown table pasted into an issue.
function Write-Utf8 {
    param([string]$Path, [string[]]$Lines)
    $enc = New-Object System.Text.UTF8Encoding($false)
    [System.IO.File]::WriteAllLines($Path, $Lines, $enc)
}

function Invoke-Native {
    param([string]$Exe, [string[]]$Arguments)
    $out = ''
    try {
        $out = (& $Exe @Arguments 2>&1 | Out-String)
    } catch {
        $out = "ERROR: $($_.Exception.Message)"
    }
    return $out
}

# ---------------------------------------------------------------------------
# Read the log
# ---------------------------------------------------------------------------

$startTime = $null
if ($Since) {
    try { $startTime = [datetime]::Parse($Since).ToUniversalTime() }
    catch { Write-Note "could not read -Since '$Since'; reading the whole log"; $startTime = $null }
}

Write-Note "reading $CiLog on $script:HostName"
$events = Get-CiEvents -StartTime $startTime
Write-Note ("{0} event(s) in scope" -f $events.Count)

$parsed = @(foreach ($e in $events) {
    [pscustomobject]@{
        Id   = $e.Id
        Utc  = $e.TimeCreated.ToUniversalTime()
        Data = (ConvertTo-EventData -Record $e)
    }
})

# 3118 is keyed by SHA256FlatHash, 3077/3076 by 'SHA256 Flat Hash'. Both name
# the same bytes, so that is the join key -- not the path, which differs
# between a staging copy and the installed copy of one build, and not the
# event count, which varies with how many times something was launched.
function Get-FlatHash {
    param($Data)
    return (Get-Field -Data $Data -Names @('SHA256 Flat Hash', 'SHA256FlatHash'))
}

$details = @($parsed | Where-Object { $_.Id -eq 3118 })
$signatures = @($parsed | Where-Object { $_.Id -eq 3089 })

# Nearest 3118 for a block, by hash, within a few seconds. CodeIntegrity writes
# the pair back to back; a wider window would join a block to a LATER attempt's
# details and quietly report the wrong verdict.
function Find-Companion {
    param($Rows, [string]$FlatHash, [datetime]$Utc, [double]$WindowSeconds = 10)
    if (-not $FlatHash) { return $null }
    $best = $null
    $bestDelta = [double]::MaxValue
    foreach ($r in $Rows) {
        $h = Get-FlatHash -Data $r.Data
        if ($h -ne $FlatHash) { continue }
        $delta = [math]::Abs(($r.Utc - $Utc).TotalSeconds)
        if ($delta -le $WindowSeconds -and $delta -lt $bestDelta) {
            $best = $r
            $bestDelta = $delta
        }
    }
    return $best
}

# 3090 is here because on a host with TestFlags=0x300 it is the ONLY record of
# an allow. Measured on a Smart App Control host after the registry change:
# a fresh copy of an unsigned binary -- no cached claim in its extended
# attribute -- produces 3090 with PassesSmartlocker=true and DefenderTrust=0,
# where a refusal of the same kind of file carries DefenderTrust=-16777216.
# A file that still HAS a cached claim produces nothing at all: the extended
# attribute answers and the graph is never asked. So "no event" means "allowed
# from this device's cache", and only a fresh copy makes the graph speak.
$verdictIds = @(3033, 3076, 3077, 3090, 3091, 3092)
$rows = @()
foreach ($p in ($parsed | Where-Object { $verdictIds -contains $_.Id })) {
    $d = $p.Data
    # Each event id spells the path differently: 3077 uses 'File Name', 3118
    # 'FileNameBuffer', 3090 'FileName'.
    $ntPath = Get-Field -Data $d -Names @('File Name', 'FileNameBuffer', 'FileName')
    $flat = Get-FlatHash -Data $d
    $det = Find-Companion -Rows $details -FlatHash $flat -Utc $p.Utc
    $sig = Find-Companion -Rows $signatures -FlatHash $flat -Utc $p.Utc
    $dd = $null
    if ($det) { $dd = $det.Data }

    $rows += [pscustomobject]@{
        Utc            = $p.Utc.ToString('o')
        EventId        = $p.Id
        Verdict        = $(switch ($p.Id) {
                              3076 { 'audited' }
                              3090 { $(if ((Get-Field -Data $d -Names @('PassesSmartlocker')) -eq 'true') { 'allowed by the graph' } else { 'allowed, not by the graph' }) }
                              3091 { 'audited (no graph authorization)' }
                              3092 { 'refused (no graph authorization)' }
                              default { 'refused' }
                          })
        Smartlocker    = (Get-Field -Data $d -Names @('PassesSmartlocker'))
        SmartlockerOn  = (Get-Field -Data $d -Names @('SmartlockerEnabled'))
        GraphTrust     = (Get-Field -Data $d -Names @('DefenderTrust'))
        FileKey        = (Get-FileKey -NtPath $ntPath)
        FilePath       = (Protect-Text (Get-Field -Data $d -Names @('File Name', 'FileNameBuffer', 'FileName')))
        ProcessPath    = (Protect-Text (Get-Field -Data $d -Names @('Process Name', 'ProcessNameBuffer')))
        Sha256         = (Get-Field -Data $d -Names @('SHA256 Hash'))
        Sha256Flat     = $flat
        Sha1           = (Get-Field -Data $d -Names @('SHA1 Hash'))
        Sha1Flat       = (Get-Field -Data $d -Names @('SHA1 Flat Hash'))
        RequestedLevel = (Get-Field -Data $d -Names @('Requested Signing Level'))
        ValidatedLevel = (Get-Field -Data $d -Names @('Validated Signing Level'))
        Status         = (Get-Field -Data $d -Names @('Status'))
        PolicyName     = (Get-Field -Data $d -Names @('PolicyName'))
        PolicyGuid     = (Get-Field -Data $d -Names @('PolicyGUID'))
        FileVersion    = (Get-Field -Data $d -Names @('FileVersion'))
        ProductName    = (Get-Field -Data $d -Names @('ProductName'))
        OriginalName   = (Get-Field -Data $d -Names @('OriginalFileName'))
        # The reputation half. Absent fields stay $null rather than being
        # invented: not every Windows build writes the same set.
        Reputation     = $(if ($dd) { $dd } else { $null })
        Signature      = $(if ($sig) { $sig.Data } else { $null })
    }
}

$allRowCount = $rows.Count
if ($Match) {
    $rows = @($rows | Where-Object { $_.FileKey -match $Match })
    Write-Note ("-Match '{0}' kept {1} of {2} verdict(s)" -f $Match, $rows.Count, $allRowCount)
}

Write-Note ("{0} verdict event(s); {1} carried Smart App Control details" -f
    $rows.Count, @($rows | Where-Object { $_.Reputation }).Count)

# ---------------------------------------------------------------------------
# One line per (file, content hash): the shape #1191's hypotheses are about
# ---------------------------------------------------------------------------

$summary = @()
# Only the refusal-shaped events carry the four hashes, so the per-hash summary
# is built from those; the 3090 rows are in Verdicts and are what dates an allow.
foreach ($g in ($rows | Where-Object { $_.Sha256Flat -and @(3033, 3076, 3077) -contains $_.EventId } | Group-Object FileKey, Sha256Flat)) {
    $items = @($g.Group | Sort-Object Utc)
    $first = $items[0]
    $last = $items[$items.Count - 1]
    $cloudCalls = 0
    $unfriendly = 0
    $lookupFailures = 0
    foreach ($i in $items) {
        if (-not $i.Reputation) { continue }
        $http = Get-Field -Data $i.Reputation -Names @('DefenderCloudHTTPCode')
        if ($http -and $http -ne '0x0' -and $http -ne '0') { $cloudCalls++ }
        if ((Get-Field -Data $i.Reputation -Names @('IsUnfriendlyFile')) -eq 'true') { $unfriendly++ }
        $sc = Get-Field -Data $i.Reputation -Names @('DefenderStatusCode')
        if ($sc -and $sc -ne '0x0' -and $sc -ne '0') { $lookupFailures++ }
    }
    $summary += [pscustomobject]@{
        FileKey        = $first.FileKey
        Sha256Flat     = $first.Sha256Flat
        Attempts       = $items.Count
        FirstUtc       = $first.Utc
        LastUtc        = $last.Utc
        SpanHours      = [math]::Round((([datetime]$last.Utc) - ([datetime]$first.Utc)).TotalHours, 2)
        CloudAnswers   = $cloudCalls
        CachedAnswers  = ($items.Count - $cloudCalls)
        UnfriendlyHits = $unfriendly
        LookupFailures = $lookupFailures
        FileVersion    = $first.FileVersion
        ProductName    = $first.ProductName
    }
}
$summary = @($summary | Sort-Object FirstUtc)

# ---------------------------------------------------------------------------
# Host state -- only meaningful at the time of an attempt
# ---------------------------------------------------------------------------

function Get-HostState {
    param([string[]]$Files)

    $sac = $null
    try {
        $sac = Get-ItemProperty 'HKLM:\SYSTEM\CurrentControlSet\Control\CI\Policy' -ErrorAction Stop |
            Select-Object VerifiedAndReputablePolicyState, SAC_EnforcementReason, SAC_PreviousState,
                          EmodePolicyRequired, SkuPolicyRequired
    } catch { $sac = "ERROR: $($_.Exception.Message)" }

    $dg = $null
    try {
        $dg = Get-CimInstance -Namespace root/Microsoft/Windows/DeviceGuard -ClassName Win32_DeviceGuard -ErrorAction Stop |
            Select-Object CodeIntegrityPolicyEnforcementStatus, UsermodeCodeIntegrityPolicyEnforcementStatus,
                          VirtualizationBasedSecurityStatus
    } catch { $dg = "ERROR: $($_.Exception.Message)" }

    # Defender is the service that answers the reputation question
    # (learn.microsoft.com .../appcontrol-debugging-and-troubleshooting). If it
    # is off or in passive mode, "unknown" is what every lookup returns, and a
    # refusal says nothing about the file.
    $mp = $null
    try {
        $mp = Get-MpComputerStatus -ErrorAction Stop |
            Select-Object AMRunningMode, AMServiceEnabled, AntivirusEnabled, IsTamperProtected,
                          RealTimeProtectionEnabled, AMEngineVersion, AntivirusSignatureVersion
    } catch { $mp = "ERROR: $($_.Exception.Message)" }

    $pref = $null
    try {
        $pref = Get-MpPreference -ErrorAction Stop |
            Select-Object MAPSReporting, SubmitSamplesConsent, CloudBlockLevel,
                          CloudExtendedTimeout, DisableRealtimeMonitoring
    } catch { $pref = "ERROR: $($_.Exception.Message)" }

    $os = $null
    try {
        $cv = Get-ItemProperty 'HKLM:\SOFTWARE\Microsoft\Windows NT\CurrentVersion' -ErrorAction Stop
        $os = [ordered]@{
            ProductName    = $cv.ProductName
            DisplayVersion = $cv.DisplayVersion
            Build          = "$($cv.CurrentBuild).$($cv.UBR)"
        }
    } catch { $os = "ERROR: $($_.Exception.Message)" }

    # NOT $files: PowerShell variable names are case-insensitive, so assigning
    # to $files here would empty the $Files parameter before the loop reads it.
    $fileRows = @()
    foreach ($f in $Files) {
        $entry = [ordered]@{ Path = (Protect-Text $f); Exists = $false }
        if (Test-Path -LiteralPath $f) {
            $entry['Exists'] = $true
            try {
                $entry['Sha256'] = (Get-FileHash -LiteralPath $f -Algorithm SHA256).Hash
                $fi = Get-Item -LiteralPath $f
                $entry['Bytes'] = $fi.Length
                $entry['WrittenUtc'] = $fi.LastWriteTimeUtc.ToString('o')
            } catch { $entry['HashError'] = $_.Exception.Message }
            # The extended attribute that caches an allow. Microsoft documents
            # $KERNEL.SMARTLOCKER.ORIGINCLAIM; a 25H2 host was measured writing
            # $KERNEL.PURGE.ESBCACHE instead, so the names are listed, never
            # assumed.
            $ea = Invoke-Native -Exe 'fsutil.exe' -Arguments @('file', 'queryEA', $f)
            $entry['ExtendedAttributes'] = (Protect-Text $ea)
            # fsutil prints its labels in the OS display language ("EA Name" on
            # an English host, "EA 名" on a Japanese one), so the raw text above
            # is evidence, not something to parse. The attribute NAMES are not
            # localised, so read those instead and give every host the same key.
            $entry['ExtendedAttributeNames'] = @([regex]::Matches($ea, '\$KERNEL\.[A-Z0-9._]+') |
                ForEach-Object { $_.Value } | Sort-Object -Unique)
            # An alternate data stream cannot be probed with Test-Path: the
            # "<path>:<stream>" form throws NotSupportedException rather than
            # answering false (measured -- it printed a red error on every
            # -Attempt run). Ask for the stream and treat its absence as the
            # error it is.
            try {
                $entry['ZoneIdentifier'] = (Get-Content -LiteralPath $f -Stream 'Zone.Identifier' -ErrorAction Stop) -join "`n"
            } catch {
                # no mark-of-the-web on this file, which is itself worth saying
                $entry['ZoneIdentifier'] = ''
            }
        }
        $fileRows += [pscustomobject]$entry
    }

    return [ordered]@{
        Os              = $os
        SacRegistry     = $sac
        DeviceGuard     = $dg
        DefenderStatus  = $mp
        DefenderPrefs   = $pref
        AppIdService    = (Invoke-Native -Exe 'sc.exe' -Arguments @('query', 'appidsvc'))
        AppLockerFilter = (Invoke-Native -Exe 'sc.exe' -Arguments @('query', 'applockerfltr'))
        Files           = $fileRows
    }
}

# ---------------------------------------------------------------------------
# Write it out
# ---------------------------------------------------------------------------

if (-not $OutDir) { $OutDir = (Get-Location).Path }
if (-not (Test-Path -LiteralPath $OutDir)) {
    New-Item -ItemType Directory -Path $OutDir -Force | Out-Null
}

$hostTag = if ($NoRedact) { $script:HostName } else { 'host' }
$stamp = (Get-Date).ToUniversalTime().ToString('yyyyMMddTHHmmssZ')
$parts = @('sac-verdict', $hostTag, $stamp)
if ($Label) { $parts += ($Label -replace '[^A-Za-z0-9._-]', '-') }
$prefix = Join-Path $OutDir ($parts -join '-')

$hostState = $null
if ($PSCmdlet.ParameterSetName -eq 'Attempt') {
    # `powershell.exe -File script.ps1 -Attempt a,b,c` binds the whole thing as
    # ONE string -- -File does not parse PowerShell syntax in its arguments.
    # Splitting here means the documented invocation works from cmd, from bash,
    # and from a PowerShell prompt alike.
    $files = @()
    foreach ($a in $Attempt) { $files += ($a -split ',' | Where-Object { $_ }) }
    Write-Note ("collecting host state for {0} file(s)" -f $files.Count)
    $hostState = Get-HostState -Files $files
}

# The policy list is long, it is the same on every run, and inlining it pushes
# the interesting table off the end of what anyone reads. Its own file.
$policyPath = "$prefix-policies.json"
$policyRaw = Invoke-Native -Exe 'citool.exe' -Arguments @('-lp', '--json')
Write-Utf8 -Path $policyPath -Lines @(Protect-Text $policyRaw)

$report = [ordered]@{
    Tool          = 'scripts/dev/sac-verdict.ps1'
    Mode          = $PSCmdlet.ParameterSetName
    CollectedUtc  = (Get-Date).ToUniversalTime().ToString('o')
    Host          = $(if ($NoRedact) { $script:HostName } else { '<host>' })
    Redacted      = (-not $NoRedact)
    Since         = $(if ($startTime) { $startTime.ToString('o') } else { $null })
    EventsInScope = $events.Count
    PolicyListFile = (Split-Path -Leaf $policyPath)
    HostState     = $hostState
    ByFileAndHash = $summary
    Verdicts      = $rows
}
$jsonPath = "$prefix.json"
Write-Utf8 -Path $jsonPath -Lines @($report | ConvertTo-Json -Depth 8)

# The Markdown is what gets pasted into the issue, so it carries the columns
# the hypotheses in #1191 are stated in, and nothing else.
$md = New-Object System.Collections.Generic.List[string]
$md.Add("# Smart App Control verdicts")
$md.Add("")
$md.Add("Collected $((Get-Date).ToUniversalTime().ToString('o')) from ``$CiLog``.")
$md.Add("$($events.Count) event(s) in scope; $($rows.Count) verdict(s); $(@($rows | Where-Object { $_.Reputation }).Count) with Smart App Control details.")
$md.Add("")
$md.Add("## By file and content hash")
$md.Add("")
$md.Add("| file | sha256 flat (8) | attempts | first (UTC) | last (UTC) | span h | cloud | cached | unfriendly | lookup fail | PE version |")
$md.Add("|---|---|---:|---|---|---:|---:|---:|---:|---:|---|")
foreach ($s in $summary) {
    $h8 = if ($s.Sha256Flat) { $s.Sha256Flat.Substring(0, [math]::Min(8, $s.Sha256Flat.Length)) } else { '' }
    $md.Add("| $($s.FileKey) | $h8 | $($s.Attempts) | $($s.FirstUtc) | $($s.LastUtc) | $($s.SpanHours) | $($s.CloudAnswers) | $($s.CachedAnswers) | $($s.UnfriendlyHits) | $($s.LookupFailures) | $($s.FileVersion) |")
}
$md.Add("")
$md.Add("## Every verdict")
$md.Add("")
$md.Add("| UTC | id | verdict | file | sha256 flat (8) | policy | graph | trust | cached trust | cloud HTTP | unfriendly |")
$md.Add("|---|---:|---|---|---|---|---|---|---|---|---|")
foreach ($r in ($rows | Sort-Object Utc)) {
    $h8 = if ($r.Sha256Flat) { $r.Sha256Flat.Substring(0, [math]::Min(8, $r.Sha256Flat.Length)) } else { '' }
    # DefenderCalled is read by nothing: this table has eleven columns and none
    # of them is it, so the extraction that used to sit here produced a value
    # that went straight in the bin (waired-agent#1224). The field is still in
    # the JSON next to this file -- $r.Reputation is written whole -- so adding
    # a column is a formatting change, not a data one, if a run ever needs it.
    $trust = ''; $cached = ''; $http = ''; $unfriendly = ''
    if ($r.Reputation) {
        $trust      = Get-Field -Data $r.Reputation -Names @('DefenderTrust')
        $cached     = Get-Field -Data $r.Reputation -Names @('CachedDefenderTrust')
        $http       = Get-Field -Data $r.Reputation -Names @('DefenderCloudHTTPCode')
        $unfriendly = Get-Field -Data $r.Reputation -Names @('IsUnfriendlyFile')
    }
    # A 3090 row has no reputation block of its own -- what it carries is
    # PassesSmartlocker, and DefenderTrust on the event itself.
    $graph = $r.Smartlocker
    if (-not $trust) { $trust = $r.GraphTrust }
    $md.Add("| $($r.Utc) | $($r.EventId) | $($r.Verdict) | $($r.FileKey) | $h8 | $($r.PolicyName) | $graph | $trust | $cached | $http | $unfriendly |")
}
$mdPath = "$prefix.md"
Write-Utf8 -Path $mdPath -Lines $md

Write-Note "wrote $jsonPath"
Write-Note "wrote $mdPath"
Write-Note "wrote $policyPath"

if ($rows.Count -eq 0) {
    Write-Note 'no verdicts in scope. That is a real answer only if this host has an application-control policy at all -- check the SAC registry values in the JSON before reading it as "nothing was ever refused".'
}

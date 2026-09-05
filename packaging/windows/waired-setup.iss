; Inno Setup 6 script for the Waired Windows GUI installer.
;
; Builds a single self-extracting WairedSetup-<ver>-x64.exe that:
;   - elevates to Administrator
;   - runs waired.exe / waired-agent.exe / waired-tray.exe from a staging
;     directory FIRST, and stops if Windows will not run one of them
;     (docs/decisions/20260829/1730-installer-refuses-programs-that-cannot-run.md)
;   - extracts those same three programs to %ProgramFiles%\Waired\
;   - runs `waired-agent.exe install` + `start` so the Go side handles SCM
;     registration, Event Log source, and the restrictive DACL on
;     %ProgramData%\waired\secrets (no duplicated logic here), and fails the
;     installation if the service does not end up running
;   - only then points Claude Code at Waired
;   - drops a Start Menu entry for "Waired" (the tray)
;   - on uninstall, runs `waired-agent.exe uninstall`
;
; PrepareToInstall is the ONLY place this script can decline. Measured against
; the Inno Setup 6 sources and recorded in
; docs/knowledges/20260904/0210-inno-can-only-decline-before-it-installs.md:
;
;   PrepareToInstall        returning a message stops Setup with exit code 7
;                           before anything is created, stopped or replaced.
;                           Under /VERYSILENT the message goes to the log and a
;                           suppressible box, so an unattended run ends.
;   [Files] AfterInstall    CANNOT fail the installation. Inno catches the
;                           exception on purpose -- "Don't allow exceptions
;                           raised by Before/AfterInstall functions to be
;                           propagated out" (Setup.MainFunc.pas
;                           NotifyInstallEntry).
;   [Run]                   CANNOT fail it either: Inno discards the result.
;   ssPostInstall           CANNOT fail it either: SetStep(ssPostInstall, True)
;                           handles the exception and carries on.
;   [Files] Check           evaluated twice (CalcFilesSize on the Ready page and
;                           again in CopyFiles), so it cannot carry work.
;
; So everything that may fail happens in PrepareToInstall, including placing
; waired-agent.exe and bringing its service up. That is also why this script
; installs waired-agent.exe itself rather than through [Files]: the service has
; to be running before Setup has committed to anything.
;
; waired-agent#1181 is what this is about: a service registration blocked by
; Smart App Control was logged as `CreateProcess failed; code 4551.`, Setup
; carried on, enabled the Claude Code integration and reported success --
; leaving Claude Code pointed at a gateway that would never listen.
;
; AppId is the immutable identity Inno Setup uses to detect prior
; versions for upgrades. NEVER change it -- if it changes between
; releases, Inno treats the old install as a separate app and leaves
; both side-by-side. Generated once for this project.
;
; Build (from a Windows host with Inno Setup 6 installed):
;   iscc /DAppVersion=1.2.3 packaging\windows\waired-setup.iss
;
; The release.yml `build (windows/amd64)` job invokes this after
; `make dist-windows-installer` has staged the three exes into
; dist\windows-amd64\.

#ifndef AppVersion
  #define AppVersion "0.0.0-dev"
#endif

; WinFileVersion is AppVersion reduced to the four integers a Windows
; VERSIONINFO resource needs -- scripts/ci/win-fileversion.sh produces it, and
; the callers that pass /DAppVersion pass this alongside. Two separate defines
; because Inno keeps the two apart and so does Windows: VersionInfoVersion runs
; through StrToVersionNumbers and is a COMPILE ERROR on anything that is not
; numeric (Compiler.SetupCompiler.pas, is-6_7_3), while AppVersion here is
; always a semver with a prerelease -- 0.0.3-rc1, or an edge build's
; 0.0.3-edge.<ts>+<sha>. The free-text half goes in VersionInfoTextVersion.
#ifndef WinFileVersion
  #define WinFileVersion "0.0.0.0"
#endif

[Setup]
AppId={{B4F8A1C2-3D5E-4F6A-9B8C-7D1E2F3A4B5C}
AppName=Waired
AppVersion={#AppVersion}
AppVerName=Waired {#AppVersion}
AppPublisher=Waired
AppPublisherURL=https://github.com/waired-ai/waired
AppSupportURL=https://github.com/waired-ai/waired/issues
DefaultDirName={autopf}\Waired
DisableDirPage=yes
DefaultGroupName=Waired
DisableProgramGroupPage=yes
PrivilegesRequired=admin
ArchitecturesAllowed=x64compatible
ArchitecturesInstallIn64BitMode=x64compatible
WizardStyle=modern
OutputDir=dist
OutputBaseFilename=WairedSetup-{#AppVersion}-x64
; The setup program is a shipped Windows PE too, and it was reporting file
; version 0.0.0.0 like the three it installs (waired-agent#1209). Inno does
; NOT default VersionInfoVersion from AppVersion -- with the directive absent
; the numeric version is left at zero, and VersionInfoTextVersion, which is
; what Explorer's Properties dialog shows, defaults to VersionInfoVersion's
; own text and so was blank. Description / ProductName / Company are left to
; their defaults, which Inno already takes from AppName and AppPublisher
; ("Waired Setup" / "Waired" / "Waired").
VersionInfoVersion={#WinFileVersion}
VersionInfoTextVersion={#AppVersion}
VersionInfoOriginalFileName=WairedSetup-{#AppVersion}-x64.exe
; Always write a log. An install that stops because Windows refused a program
; -- or one that succeeded and left something odd -- is diagnosed from this
; file, and waired-agent#1181 was only readable because the run happened to
; have one. Inno puts it in %TEMP%\Setup Log*.txt unless /LOG=<file> names one.
SetupLogging=yes
; The install stage is a few seconds of copying: everything that can fail has
; already happened in PrepareToInstall, and by then the service is registered
; and running. A cancel in that window would roll back files Inno owns and
; leave the service it does not know about, so there is nothing to gain by
; offering it.
AllowCancelDuringInstall=no
; NoCompression is for throwaway builds only: the install test compiles
; deliberately broken payloads to prove Setup stops on them, and pays a minute
; of lzma2/ultra per build otherwise. Shipping builds never define it.
#ifdef NoCompression
Compression=none
SolidCompression=no
#else
Compression=lzma2/ultra
SolidCompression=yes
#endif
; Use the existing tray "connected" icon for both the installer's own
; icon and the Add/Remove Programs entry. A larger / hi-res icon can
; replace this later without touching the rest of the install flow.
; Path is SourceDir-relative (= repo root, see SourceDir below) — Inno
; Setup resolves [Setup] file paths against SourceDir once it has been
; set, not against the .iss's own directory.
SetupIconFile=internal\gui\tray\icons\waired-connected.ico
UninstallDisplayIcon={app}\waired-tray.exe
; Paths below are resolved relative to this directory: the repo root,
; one level above packaging\windows.
SourceDir=..\..

[Languages]
Name: "english"; MessagesFile: "compiler:Default.isl"

[Files]
; The three programs are embedded ONCE, with dontcopy, so PrepareToInstall can
; extract and actually run them before anything on this computer is created,
; stopped or replaced. They are then installed from those extracted copies with
; `external`, which is why there is no second copy inside the setup executable.
; With solid compression the extraction reads the stream in order, so these
; come first. ExternalSize is deliberately not set: the destination page is
; disabled, so the figure is never shown, and the alternative is a compile-time
; path expression that is easy to get silently wrong.
Source: "dist\windows-amd64\waired.exe";       Flags: dontcopy noencryption
Source: "dist\windows-amd64\waired-agent.exe"; Flags: dontcopy noencryption
Source: "dist\windows-amd64\waired-tray.exe";  Flags: dontcopy noencryption

; waired-agent.exe is deliberately NOT here: PrepareToInstall places it and
; brings its service up, because that is the last moment Setup can still
; decline (see the header). [UninstallDelete] removes it.
Source: "{tmp}\waired.exe";      DestDir: "{app}"; Flags: external ignoreversion
Source: "{tmp}\waired-tray.exe"; DestDir: "{app}"; Flags: external ignoreversion
Source: "dist\windows-amd64\VERSION";              DestDir: "{app}"; Flags: ignoreversion
Source: "dist\windows-amd64\LICENSE";              DestDir: "{app}"; Flags: ignoreversion
Source: "dist\windows-amd64\THIRD_PARTY_LICENSES"; DestDir: "{app}"; Flags: ignoreversion

[Icons]
Name: "{group}\Waired";            Filename: "{app}\waired-tray.exe"
Name: "{group}\Waired (CLI)";      Filename: "cmd.exe"; Parameters: "/k ""{app}\waired.exe"" --help"

[Registry]
; Record the install location where install.ps1 / uninstall.ps1 look for it
; (install.ps1 -InstallDir writes the same value), so the script uninstaller
; and script updates find a GUI install even at a non-default directory.
Root: HKLM; Subkey: "SOFTWARE\Waired"; ValueType: string; ValueName: "InstallDir"; \
    ValueData: "{app}"; Flags: uninsdeletekey

[Tasks]
; Default-checked, mirroring the Linux installer's default-on Claude
; integration (with disclosure). Unchecking it leaves Claude Code routing
; straight to api.anthropic.com; it can be enabled later with an elevated
; `waired claude enable`. Task name kept as "claudeproxy" for upgrade
; continuity even though the mechanism is now managed settings, not a proxy.
;
; NOTE: this GUI installer does NOT run `waired init` (enrolment happens
; later via the tray / CLI), so this checkbox is the ONLY place Claude
; routing is decided in the GUI flow. It is therefore NOT the duplicate of
; init's "Route now?" prompt that the CLI installer (install.ps1) dropped:
; there the post-init `waired claude enable` step overrode an interactive
; "no" and was removed, with the choice forwarded into init via
; --skip-claude-route. Do not remove the claude-enable step in
; CurStepChanged(ssPostInstall) — that would leave GUI installs unrouted.
Name: "claudeproxy"; \
    Description: "Route Claude Code through Waired via Claude Code managed settings (points ANTHROPIC_BASE_URL at local inference, no credential; in Claude Code, /model then picks where each turn runs). No CA certificate or hosts-file change."; \
    GroupDescription: "Claude Code integration:"

[Run]
; Optional: launch the tray immediately after install so its first
; run can write its HKCU\...\Run autostart entry via
; internal/platform/autostart/autostart_windows.go.
;
; Nothing else lives here. [Run] entries are executed after the install stage
; has been committed, and Inno discards their result -- a failed one leaves
; Setup reporting success. The service registration and the Claude Code
; integration live in the script section below, for that reason.
Filename: "{app}\waired-tray.exe"; \
    Description: "Launch Waired now (recommended -- registers per-user autostart)"; \
    Flags: nowait postinstall skipifsilent runasoriginaluser

[UninstallRun]
; Run BEFORE files are removed so the exes still exist. Disable the Claude
; Code integration first (while waired.exe + the agent service are still
; present): removes the managed-settings ANTHROPIC_BASE_URL and sweeps any
; residual retired-MITM artifacts (hosts redirect, Root-store CA,
; NODE_EXTRA_CA_CERTS). Idempotent — a no-op when it was never enabled.
; Replaces the removed `waired proxy uninstall` (waired#750).
Filename: "{app}\waired.exe"; Parameters: "claude disable"; \
    Flags: runhidden waituntilterminated; \
    RunOnceId: "WairedClaudeDisable"
Filename: "{app}\waired-agent.exe"; Parameters: "uninstall"; \
    Flags: runhidden waituntilterminated; \
    RunOnceId: "WairedAgentUninstall"

[UninstallDelete]
; The Go-side install handler creates %ProgramData%\waired\ at first
; run; do NOT remove it on uninstall by default so a re-install
; preserves identity / keys. Users who want a clean slate can use the
; checkbox below to wipe state too.
Type: files; Name: "{app}\VERSION"
; PrepareToInstall placed this one, so Inno's uninstall log does not know it.
; The .displaced-* pattern catches an image Windows would not let Setup
; overwrite and which was renamed aside instead.
Type: files; Name: "{app}\waired-agent.exe"
Type: files; Name: "{app}\waired-agent.exe.displaced-*"
; Setup's own working directories. They are removed as soon as they have served
; their purpose, so these entries only catch a run that was killed mid-way.
Type: filesandordirs; Name: "{app}\.waired-staging"
Type: filesandordirs; Name: "{app}\.waired-rollback"

[Code]
const
  // The program that becomes the Windows Service. Setup places this one itself
  // (see the header) and everything else goes through [Files].
  AgentProgram = 'waired-agent.exe';
  // Where the programs are tried before they are installed. Directly under the
  // install directory, matching install.ps1's staging directory: they are
  // started from the path prefix they will run from, and a computer that
  // refuses to execute anything out of %TEMP% does not turn into a false
  // refusal.
  StagingDirName = '.waired-staging';
  // Where an upgrade's previous waired-agent.exe is kept while the new one goes
  // in, so a service that will not start can be put back.
  RollbackDirName = '.waired-rollback';

var
  // True when a waired-agent Windows Service was already registered when Setup
  // started, i.e. this run is an upgrade-in-place over a prior install.
  gAgentServiceExisted: Boolean;
  // True when Setup had to create the install directory just to stage into, so
  // a refusal can leave the computer exactly as it found it.
  gAppDirCreatedByPreflight: Boolean;
  // True once the service is registered AND running. Read in ssPostInstall
  // before anything is done to Claude Code.
  gAgentRunning: Boolean;

function AppDir(): String;
begin
  Result := AddBackslash(ExpandConstant('{app}'));
end;

function StagingDir(): String;
begin
  Result := AppDir() + StagingDirName;
end;

function RollbackDir(): String;
begin
  Result := AppDir() + RollbackDirName;
end;

// AgentServiceExists reports whether the waired-agent service is registered
// with the SCM, via a read-only `sc.exe query`. Exit code 0 => registered
// (any run state); 1060 (ERROR_SERVICE_DOES_NOT_EXIST) => not registered.
function AgentServiceExists(): Boolean;
var
  ResultCode: Integer;
begin
  Result := False;
  if Exec(ExpandConstant('{sys}\sc.exe'), 'query waired-agent', '',
          SW_HIDE, ewWaitUntilTerminated, ResultCode) then
    Result := (ResultCode = 0);
end;

// AgentServiceIsRunning asks the SCM to interrogate the service. Exit codes
// only, so nothing parses localised `sc query` output: 0 means the service
// answered, 1062 (ERROR_SERVICE_NOT_ACTIVE) that it is registered but not
// running, 1060 that it is not registered at all.
//
// It is here because `waired-agent.exe start` REPORTS whether the service came
// up, and a report is not a state -- the distinction waired-agent#1087 and
// #1181 are both about. Measured: a stand-in binary that exits 0 without ever
// becoming a service passes the report and fails this.
//
// Three attempts: `start` has already waited for Running, so one answer is
// normally enough, but the SCM can still be settling and a single transient
// no is not worth failing an install over.
function AgentServiceIsRunning(): Boolean;
var
  ResultCode, Attempt: Integer;
begin
  Result := False;
  for Attempt := 1 to 3 do begin
    if Exec(ExpandConstant('{sys}\sc.exe'), 'interrogate waired-agent', '',
            SW_HIDE, ewWaitUntilTerminated, ResultCode) and (ResultCode = 0) then begin
      Result := True;
      Exit;
    end;
    Sleep(1000);
  end;
end;

// WhyItWillNotRun starts Path with Params and returns '' when Windows ran it,
// or the reason it did not.
//
// Exec returns False when CreateProcess itself failed and puts GetLastError in
// ResultCode (Setup.InstFunc.pas) -- 4551 is what an application-control
// refusal looks like there. SysErrorMessage turns that into the OS's own words,
// which is the text install.ps1 surfaces through Win32Exception, so both
// installers say the same thing about the same computer.
function WhyItWillNotRun(const Path, Params: String; const RequireZeroExit: Boolean): String;
var
  ResultCode: Integer;
begin
  Result := '';
  if not FileExists(Path) then begin
    Result := 'it is not in this installer';
    Exit;
  end;
  if not Exec(Path, Params, ExtractFileDir(Path), SW_HIDE, ewWaitUntilTerminated, ResultCode) then begin
    Result := Trim(SysErrorMessage(ResultCode));
    if Result = '' then
      Result := Format('Windows would not start it (error %d)', [ResultCode]);
    Exit;
  end;
  if RequireZeroExit and (ResultCode <> 0) then
    Result := Format('it ran but exited with code %d', [ResultCode]);
end;

// RunInstalledProgram runs one of the installed programs and returns '' when it
// succeeded, or the reason it did not -- Windows refusing to start it and the
// program itself failing are both reported, because ignoring either is what
// waired-agent#1181 was.
function RunInstalledProgram(const Name, Params: String): String;
var
  Path: String;
  ResultCode: Integer;
begin
  Result := '';
  Path := AppDir() + Name;
  if not Exec(Path, Params, ExpandConstant('{app}'), SW_HIDE, ewWaitUntilTerminated, ResultCode) then begin
    Result := Trim(SysErrorMessage(ResultCode));
    if Result = '' then
      Result := Format('Windows would not start %s (error %d)', [Name, ResultCode]);
    Exit;
  end;
  if ResultCode <> 0 then
    Result := Format('`%s %s` exited with code %d', [Name, Params, ResultCode]);
end;

// ProgramName returns the name of shipped program I (0..2).
function ProgramName(const I: Integer): String;
begin
  case I of
    0: Result := 'waired.exe';
    1: Result := AgentProgram;
  else
    Result := 'waired-tray.exe';
  end;
end;

// CleanUpWorkDirs clears the directories Setup works in. AlsoRemoveAppDir puts
// back the one other thing they can leave behind: an install directory this run
// created only so it had somewhere to stage into. RemoveDir only succeeds while
// the directory is empty, which is exactly when removing it is right.
procedure CleanUpWorkDirs(const AlsoRemoveAppDir: Boolean);
begin
  DelTree(StagingDir(), True, True, True);
  DelTree(RollbackDir(), True, True, True);
  if AlsoRemoveAppDir and gAppDirCreatedByPreflight then
    RemoveDir(ExpandConstant('{app}'));
end;

// StagedCheck says what to ask one program before it is installed, and whether
// a refusal stops the install. One line per program, and
// scripts/install/waired_setup_iss_test.go holds those lines against
// packaging/install/install.ps1's Get-StagedBinaryChecks -- two copies of one
// table forget different things.
//
// The ruling behind it is
// docs/decisions/20260829/1730-installer-refuses-programs-that-cannot-run.md:
// without the daemon there is no product, and without the CLI there is no
// `waired init`, `waired doctor` or `waired update`, so nobody could finish or
// diagnose the install. A refused Waired app costs the app, not the computer,
// so that one only warns.
//
// Never a bare word as an argument to waired-agent: its flag parsing stops at
// the first non-flag token, so `waired-agent.exe version` would start the
// daemon in the foreground and sit there. `-h` exits 1 by design
// (flag.ContinueOnError), which is why its exit code is not read. waired.exe is
// asked for `version --json` and its exit code IS read, because a program that
// starts and then cannot report its own version is not one to install either.
procedure StagedCheck(const Name: String; var Params: String; var RequireZeroExit, Fatal: Boolean);
begin
  // A program this table does not know: ask the least of it, and let a refusal
  // stop the install rather than pass unnoticed.
  Params := '-h'; RequireZeroExit := False; Fatal := True;
  if Name = 'waired.exe'       then begin Params := 'version --json'; RequireZeroExit := True;  Fatal := True;  end;
  if Name = 'waired-agent.exe' then begin Params := '-h';             RequireZeroExit := False; Fatal := True;  end;
  if Name = 'waired-tray.exe'  then begin Params := '-h';             RequireZeroExit := False; Fatal := False; end;
end;

// CheckProgramsRunHere puts the three programs where they will run from and
// starts each one. It returns '' when the install may go ahead, or the message
// the user should see instead.
function CheckProgramsRunHere(): String;
var
  I: Integer;
  Name, Params, Staged, Why: String;
  RequireZeroExit, Fatal: Boolean;
begin
  Result := '';
  gAppDirCreatedByPreflight := not DirExists(ExpandConstant('{app}'));
  if not ForceDirectories(StagingDir()) then begin
    Result := 'Setup could not create ' + StagingDir() + '.';
    Exit;
  end;

  // Same sentence install.ps1 prints, so both installers say the same thing
  // and docs-site can quote one line for both.
  Log('Checking the new programs run on this computer before replacing anything');
  for I := 0 to 2 do begin
    Name := ProgramName(I);
    StagedCheck(Name, Params, RequireZeroExit, Fatal);

    ExtractTemporaryFile(Name);
    Staged := AddBackslash(StagingDir()) + Name;
    if not FileCopy(ExpandConstant('{tmp}\') + Name, Staged, False) then begin
      Result := Format('Setup could not put %s where it can be tried.', [Name]);
      Exit;
    end;

    Why := WhyItWillNotRun(Staged, Params, RequireZeroExit);
    if Why = '' then
      Continue;

    if not Fatal then begin
      LogFmt('The Waired app (%s) will not run on this computer: %s', [Name, Why]);
      SuppressibleMsgBox(
        Format('The Waired app (%s) will not run on this computer:', [Name]) + #13#10 +
        '  ' + Why + #13#10#13#10 +
        'Setup continues; the background service and the waired command are not' + #13#10 +
        'affected, but the app will not open until Windows accepts that file.',
        mbInformation, MB_OK, IDOK);
      Continue;
    end;

    Result :=
      Format('Windows will not run the new %s on this computer:', [Name]) + #13#10#13#10 +
      '  ' + Why + #13#10#13#10 +
      'Waired''s programs are not signed with a certificate Windows recognises, so' + #13#10 +
      'Smart App Control (or another application-control policy) can refuse to run' + #13#10 +
      'them. The refusal is per file and can change on its own, so a later build --' + #13#10 +
      'or the same one, later -- may be accepted.' + #13#10#13#10 +
      'Nothing has been installed, removed or replaced.';
    Log(Result);
    Exit;
  end;
end;

// SavePreviousAgent keeps a copy of the waired-agent.exe an upgrade is about to
// replace. CheckProgramsRunHere answers "will these programs run here"; it
// cannot answer "will they still be allowed to run in thirty seconds", and the
// verdict does move on its own. So the upgrade path carries its own way back
// (docs/decisions/20260829/1730-installer-refuses-programs-that-cannot-run.md).
procedure SavePreviousAgent();
var
  Source: String;
begin
  DelTree(RollbackDir(), True, True, True);
  if not ForceDirectories(RollbackDir()) then begin
    Log('Could not create ' + RollbackDir() + ': an upgrade that fails will not be able to put the previous version back.');
    Exit;
  end;
  Source := AppDir() + AgentProgram;
  if FileExists(Source) and not FileCopy(Source, AddBackslash(RollbackDir()) + AgentProgram, False) then
    Log('Could not copy ' + Source + ' aside.');
end;

function RestorePreviousAgent(): Boolean;
var
  Saved: String;
begin
  Saved := AddBackslash(RollbackDir()) + AgentProgram;
  Result := FileExists(Saved) and FileCopy(Saved, AppDir() + AgentProgram, False);
end;

// PlaceAgentProgram puts the checked waired-agent.exe at its final path and
// returns '' or the reason it could not.
//
// Windows will not overwrite a mapped image, so a copy that fails is retried
// after renaming the old file aside -- the same move install.ps1 makes
// (Set-InstallDirFile), and the reason [UninstallDelete] sweeps
// waired-agent.exe.displaced-*.
function PlaceAgentProgram(): String;
var
  Source, Dest, Aside: String;
begin
  Result := '';
  Source := AddBackslash(StagingDir()) + AgentProgram;
  Dest := AppDir() + AgentProgram;
  if FileCopy(Source, Dest, False) then
    Exit;
  Aside := Dest + '.displaced-' + GetDateTimeString('yyyymmddhhnnss', #0, #0);
  if not RenameFile(Dest, Aside) then begin
    Result := Format('%s is in use and could not be replaced', [AgentProgram]);
    Exit;
  end;
  LogFmt('%s was in use; the old copy is now %s', [AgentProgram, Aside]);
  if not FileCopy(Source, Dest, False) then
    Result := Format('%s could not be placed in %s', [AgentProgram, ExpandConstant('{app}')]);
end;

// SetUpTheService places waired-agent.exe and brings its service up, and
// returns '' or the message the user should see instead of an install.
//
// `waired-agent.exe start` waits for the service to reach Running and exits
// non-zero if it does not (internal/platform/service/service_windows.go), so
// its exit code is the answer -- no parsing of localised `sc query` output.
function SetUpTheService(): String;
var
  Why, Recovery: String;
begin
  Result := '';
  gAgentServiceExisted := AgentServiceExists();
  if gAgentServiceExisted then begin
    SavePreviousAgent();
    // Windows locks a running binary, unlike the Unix in-place swap the .deb /
    // macOS paths use. Delegate to the Go SCM logic, matching the install and
    // uninstall steps -- no duplicated service logic here.
    Why := RunInstalledProgram(AgentProgram, 'stop');
    if Why <> '' then
      Log('Could not stop the running waired-agent: ' + Why);
    Why := '';
  end;

  WizardForm.StatusLabel.Caption := 'Setting up the waired-agent Windows Service...';
  Why := PlaceAgentProgram();
  if Why = '' then begin
    // On an upgrade the service is already registered and `install` would error
    // out with "already installed"; its ImagePath already points here, so the
    // just-placed binary is what the start below brings up.
    if not gAgentServiceExisted then
      Why := RunInstalledProgram(AgentProgram, 'install');
    if Why = '' then
      Why := RunInstalledProgram(AgentProgram, 'start');
    if (Why = '') and not AgentServiceExists() then
      Why := 'the service is not registered with Windows afterwards';
    if (Why = '') and not AgentServiceIsRunning() then
      Why := 'the service is registered but is not running';
  end;

  if Why = '' then begin
    gAgentRunning := True;
    Exit;
  end;

  Log('waired-agent service setup failed: ' + Why);
  if gAgentServiceExisted then begin
    if RestorePreviousAgent() and (RunInstalledProgram(AgentProgram, 'start') = '') then
      Recovery := 'The version you had is back and its background service is running again.'
    else
      Recovery := 'Setup could not put the previous version back. Install Waired again to repair it.';
  end else begin
    // Leave no half-registered service, and no program, behind.
    RunInstalledProgram(AgentProgram, 'uninstall');
    DeleteFile(AppDir() + AgentProgram);
    Recovery := 'Nothing has been installed, and Claude Code was not changed.';
  end;

  Result := 'Waired''s background service did not start on this computer:' + #13#10#13#10 +
            '  ' + Why + #13#10#13#10 + Recovery;
end;

// PrepareToInstall is the only place this script can decline (see the header):
// returning a message stops Setup with exit code 7, before [Files], before the
// registry and Start Menu entries, and before Claude Code is looked at.
function PrepareToInstall(var NeedsRestart: Boolean): String;
begin
  Result := CheckProgramsRunHere();
  if Result = '' then
    Result := SetUpTheService();
  CleanUpWorkDirs(Result <> '');
end;

procedure CurStepChanged(CurStep: TSetupStep);
var
  Why: String;
begin
  if CurStep <> ssPostInstall then
    Exit;
  CleanUpWorkDirs(False);

  // Claude Code routing, last, and only once the daemon is actually there.
  // Writes %ProgramFiles%\ClaudeCode\managed-settings.json pointing
  // ANTHROPIC_BASE_URL at waired's local gateway (no credential, no CA, no
  // hosts edit) and sweeps any residual retired-MITM artifacts; elevated, as
  // the managed-settings write needs admin. Replaces the removed
  // `waired proxy install` (waired#750).
  //
  // waired-agent#1181: this used to be a [Run] entry that ran whatever had
  // happened to the service, so a computer whose service never registered had
  // its Claude Code pointed at a gateway that would never listen. It cannot be
  // reached now without a running service -- and when it fails on its own,
  // Claude Code simply keeps talking to api.anthropic.com, which is where it
  // was, so it says so rather than failing the install.
  if not WizardIsTaskSelected('claudeproxy') then
    Log('Claude Code integration not selected; leaving Claude Code alone.')
  else if not gAgentRunning then
    Log('Claude Code integration skipped: the waired-agent service is not running.')
  else begin
    WizardForm.StatusLabel.Caption := 'Enabling Claude Code routing (managed settings)...';
    Why := RunInstalledProgram('waired.exe', 'claude enable');
    if Why <> '' then begin
      Log('Claude Code routing was not enabled: ' + Why);
      SuppressibleMsgBox(
        'Waired is installed, but Claude Code was not pointed at it:' + #13#10#13#10 +
        '  ' + Why + #13#10#13#10 +
        'Claude Code keeps talking to api.anthropic.com. Run' + #13#10 +
        '`waired claude enable` from an Administrator terminal to try again.',
        mbError, MB_OK, IDOK);
    end;
  end;
end;

function InitializeUninstall(): Boolean;
begin
  Result := True;
end;

procedure CurUninstallStepChanged(CurUninstallStep: TUninstallStep);
var
  ProgramData: string;
  WairedState: string;
  WipeState: Boolean;
begin
  if CurUninstallStep = usPostUninstall then begin
    ProgramData := ExpandConstant('{commonappdata}');
    WairedState := ProgramData + '\waired';
    if DirExists(WairedState) then begin
      { Never block a silent uninstall on this prompt: a plain MsgBox is NOT
        suppressed by /VERYSILENT or /SUPPRESSMSGBOXES, so it used to hang
        unattended uninstalls forever on an invisible dialog (found by the
        installtest .exe variant, waired#760). Silent uninstalls keep the
        state (the safe default -- same device key on reinstall); interactive
        ones still get the question, with /SUPPRESSMSGBOXES answering No. }
      if UninstallSilent then
        WipeState := False
      else
        WipeState := SuppressibleMsgBox(
          'Remove Waired state directory?' + #13#10 + #13#10 +
          WairedState + #13#10 + #13#10 +
          'This contains the device identity, secrets, and any cached state.' + #13#10 +
          'Keep it (No) if you plan to reinstall later -- the same device key' + #13#10 +
          'will be re-used and re-enrollment is unnecessary.',
          mbConfirmation, MB_YESNO or MB_DEFBUTTON2, IDNO) = IDYES;
      if WipeState then begin
        DelTree(WairedState, True, True, True);
      end;
    end;
  end;
end;

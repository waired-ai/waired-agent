; Inno Setup 6 script for the Waired Windows GUI installer.
;
; Builds a single self-extracting WairedSetup-<ver>-x64.exe that:
;   - elevates to Administrator
;   - extracts waired.exe / waired-agent.exe / waired-tray.exe to
;     %ProgramFiles%\Waired\
;   - runs `waired-agent.exe install` so the Go side handles SCM
;     registration, Event Log source, and the restrictive DACL on
;     %ProgramData%\waired\secrets (no duplicated logic here)
;   - drops a Start Menu entry for "Waired" (the tray)
;   - on uninstall, runs `waired-agent.exe uninstall`
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
Compression=lzma2/ultra
SolidCompression=yes
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
Source: "dist\windows-amd64\waired.exe";       DestDir: "{app}"; Flags: ignoreversion
Source: "dist\windows-amd64\waired-agent.exe"; DestDir: "{app}"; Flags: ignoreversion
Source: "dist\windows-amd64\waired-tray.exe";  DestDir: "{app}"; Flags: ignoreversion
Source: "dist\windows-amd64\VERSION";          DestDir: "{app}"; Flags: ignoreversion
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
; --skip-claude-route. Do not remove the [Run] claude-enable step below —
; that would leave GUI installs unrouted.
Name: "claudeproxy"; \
    Description: "Route Claude Code through Waired via Claude Code managed settings (points ANTHROPIC_BASE_URL at local inference, no credential; transparently falls back to the real Anthropic API). No CA certificate or hosts-file change."; \
    GroupDescription: "Claude Code integration:"

[Run]
; Register the Windows Service. The Go-side install handler picks up
; its own exe path via os.Executable(), so the SCM ImagePath ends up
; pointing at {app}\waired-agent.exe (not the staging path Inno
; extracted from).
;
; Check: ShouldRegisterAgent — register ONLY on a fresh install. On an
; upgrade-in-place the service is already registered (and `install` would
; error out with "already installed"); its ImagePath already points at
; {app}\waired-agent.exe, so the just-copied binary is picked up by the
; stop/start in CurStepChanged (see [Code]) with no re-registration.
Filename: "{app}\waired-agent.exe"; Parameters: "install"; \
    Flags: runhidden waituntilterminated; \
    Check: ShouldRegisterAgent; \
    StatusMsg: "Registering waired-agent Windows Service..."

; On a fresh install, START the service right after registering it, so the
; daemon is already running when the operator later signs in with `waired
; init` (this GUI installer does not run init itself). init then attaches to
; the running agent and takes the daemon-driven onboarding path (waired#835
; §11.2) rather than the legacy standalone enroll. Fresh-only
; (ShouldRegisterAgent); on an upgrade CurStepChanged(ssPostInstall) owns the
; restart. Safe before sign-in: the daemon idles until enrolment (#177);
; parity with the .deb postinst start-on-fresh-install and install.ps1's
; Ensure-AgentRunning.
Filename: "{app}\waired-agent.exe"; Parameters: "start"; \
    Flags: runhidden waituntilterminated; \
    Check: ShouldRegisterAgent; \
    StatusMsg: "Starting waired-agent Windows Service..."

; Enable Claude Code routing via managed settings (only when the task is
; checked): writes %ProgramFiles%\ClaudeCode\managed-settings.json pointing
; ANTHROPIC_BASE_URL at waired's local gateway (no credential, no CA, no
; hosts edit) and sweeps any residual retired-MITM artifacts. Runs AFTER the
; service-register entry above; elevated, as the managed-settings write needs
; admin. Replaces the removed `waired proxy install` (waired#750).
Filename: "{app}\waired.exe"; Parameters: "claude enable"; \
    Tasks: claudeproxy; \
    Flags: runhidden waituntilterminated; \
    StatusMsg: "Enabling Claude Code routing (managed settings)..."

; Optional: launch the tray immediately after install so its first
; run can write its HKCU\...\Run autostart entry via
; internal/platform/autostart/autostart_windows.go.
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

[Code]
var
  WipeStatePage: TInputOptionWizardPage;
  // True when a waired-agent Windows Service is already registered at the
  // start of setup (i.e. this run is an upgrade-in-place over a prior
  // install). Set in CurStepChanged(ssInstall); read by ShouldRegisterAgent
  // and the ssPostInstall restart.
  gAgentServiceExisted: Boolean;

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

// ShouldRegisterAgent gates the `[Run] waired-agent install` step to fresh
// installs only. On an upgrade the service is already registered (and
// `install` would error), so we keep the existing registration and just
// restart onto the new binary in CurStepChanged(ssPostInstall).
function ShouldRegisterAgent(): Boolean;
begin
  Result := not gAgentServiceExisted;
end;

procedure CurStepChanged(CurStep: TSetupStep);
var
  ResultCode: Integer;
begin
  if CurStep = ssInstall then begin
    // Before [Files] copies: on an upgrade, stop the running agent so its
    // locked waired-agent.exe can be overwritten (Windows locks a running
    // binary, unlike the Unix in-place swap the .deb / macOS paths use).
    // Delegate to the Go SCM logic, matching the install/uninstall steps
    // (no duplicated service logic here). On a fresh install the service is
    // absent (and {app}\waired-agent.exe does not yet exist), so we skip.
    gAgentServiceExisted := AgentServiceExists();
    if gAgentServiceExisted then
      Exec(ExpandConstant('{app}\waired-agent.exe'), 'stop', '',
           SW_HIDE, ewWaitUntilTerminated, ResultCode);
  end else if CurStep = ssPostInstall then begin
    // After the new binaries are in place: restart the agent onto the new
    // exe so an upgrade never leaves the service stopped or on the old
    // binary. Fresh installs are registered AND started by the [Run] steps
    // above (so a later `waired init` attaches to the running daemon and
    // takes the daemon-driven onboarding path -- waired#835 §11.2); here we
    // cover only the upgrade path (parity with the .deb postinst
    // restart-on-upgrade and install.ps1 -Update). A no-op if the
    // proxy-install [Run] step already brought it up.
    if gAgentServiceExisted then
      Exec(ExpandConstant('{app}\waired-agent.exe'), 'start', '',
           SW_HIDE, ewWaitUntilTerminated, ResultCode);
  end;
end;

procedure InitializeWizard();
begin
  // Uninstall-time "drop config" toggle is handled in
  // CurUninstallStepChanged below. Nothing to do here at install time.
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

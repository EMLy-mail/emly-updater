; ApplicationName is the *technical* name: exe file name, install directory,
; setup file name, and - through AppId below - the uninstall registry key.
; All four are load-bearing (emly checks %ProgramFiles%\EMLyUpdater,
; emly-go-api serves EMLyUpdater_Installer_<ver>.exe to self-update, and a
; different AppId would install side by side instead of upgrading), so it
; stays EMLyUpdater. ProductName is what people see.
#define ApplicationName 'EMLyUpdater'
#define ProductName 'AryxD Agent'
#define ApplicationVersion '1.8.0'
#define ServiceName 'EMLyUpdater'

; Microsoft.WinGet.Client, the PowerShell module internal/winget drives.
; Optional component "wingetmodule", off by default. Downloaded at install
; time from PowerShell Gallery (a .nupkg is a zip) and accepted only if its
; SHA256 matches the one pinned here: this script is compiled into a signed
; setup, so the pin cannot be altered and the module gets the same guarantee
; as a file shipped inside the setup. To move to a new version, change both
; defines together:
;   Invoke-WebRequest https://www.powershellgallery.com/api/v2/package/Microsoft.WinGet.Client/<ver> -OutFile m.zip
;   (Get-FileHash m.zip -Algorithm SHA256).Hash
#define WinGetModuleName 'Microsoft.WinGet.Client'
#define WinGetModuleVersion '1.29.380'
#define WinGetModuleSHA256 '3469e5747eb6b100e51fed3f2057386b5ba60bc8955a6669b5c2eb562e316619'
#define WinGetModuleURL 'https://www.powershellgallery.com/api/v2/package/' + WinGetModuleName + '/' + WinGetModuleVersion

; PowerShell 7, installed together with the module by the same "wingetmodule"
; component - deliberately not a component of its own: the module does not
; work in the service without it (run as SYSTEM, Get-WinGetPackage refuses
; Windows PowerShell 5.1 with WindowsPowerShellNotSupported, and
; internal/winget prefers pwsh.exe when it exists), so there is no selection
; in which one without the other makes sense. Same pinning rule as the
; module; to move to a new LTS, change both defines together and take the
; hash from the release's hashes.sha256:
;   https://github.com/PowerShell/PowerShell/releases/download/v<ver>/hashes.sha256
#define PwshVersion '7.6.6'
#define PwshSHA256 '958838ff55091e1c8705d89efed0cc7e8245a3a6ef6c0ccfae20015227108ad8'
#define PwshMSI 'PowerShell-' + PwshVersion + '-win-x64.msi'
#define PwshURL 'https://github.com/PowerShell/PowerShell/releases/download/v' + PwshVersion + '/' + PwshMSI

[Setup]
; AppId must stay pinned to the old AppName: Inno Setup defaults AppId to
; AppName, so without this line the rename would orphan every existing install.
AppId={#ApplicationName}
AppName={#ProductName}
AppVersion={#ApplicationVersion}
AppVerName={#ProductName} {#ApplicationVersion}
AppPublisher=3gIT
DefaultDirName={autopf}\{#ApplicationName}
OutputBaseFilename={#ApplicationName}_Installer_{#ApplicationVersion}
ArchitecturesInstallIn64BitMode=x64compatible
DisableProgramGroupPage=yes
; The tray (EMLyUpdater.exe tray) runs the installed exe in every user
; session, so replacing the file needs it closed: the Restart Manager does it
; (the tray exits on WM_ENDSESSION) and restarts it afterwards. Both are Inno
; Setup's defaults, spelled out because the tray depends on them.
CloseApplications=yes
RestartApplications=yes
; Service registration requires elevation; deployment runs via IT tooling
; (GPO/Intune) or an admin shell anyway.
PrivilegesRequired=admin
UninstallDisplayIcon={app}\appicon.ico
WizardStyle=modern
SetupIconFile=appicon.ico
SignTool=signtool
SignedUninstaller=yes
; .zip extraction (the WinGet module's .nupkg) needs the full 7-Zip engine.
ArchiveExtraction=full

; The first type is the default, so a silent install without /COMPONENTS
; (the GPO command line) installs the updater alone. Opt in with:
;   /VERYSILENT /SUPPRESSMSGBOXES /NORESTART /NOCANCEL /COMPONENTS="updater,wingetmodule"
; ("wingetmodule" brings PowerShell 7 along.)
; /COMPONENTS replaces the selection, so list every component wanted. The
; updater's own [Files] carry no Components: parameter and are installed
; whatever the selection. Without /COMPONENTS an upgrade (self-update
; included) keeps the previous install's choice.
[Types]
Name: "compact"; Description: "{#ProductName} only"
Name: "full"; Description: "{#ProductName} + WinGet PowerShell module (with PowerShell 7)"
Name: "custom"; Description: "Custom"; Flags: iscustom

[Components]
Name: "updater"; Description: "{#ProductName} service (distributes EMLy)"; Types: compact full custom; Flags: fixed
; ExtraDiskSpaceRequired: the module's extracted size plus PowerShell 7's
; installed size (~250 MB), since nothing in [Files] accounts for either.
Name: "wingetmodule"; Description: "{#WinGetModuleName} {#WinGetModuleVersion} PowerShell module + PowerShell {#PwshVersion} (download)"; Types: full; ExtraDiskSpaceRequired: 306025934

[Files]
; Built by: go build -ldflags "-s -w" -o build\EMLyUpdater.exe .
Source: "..\build\{#ApplicationName}.exe"; DestDir: "{app}"; Flags: ignoreversion
Source: "appicon.ico"; DestDir: "{app}"; Flags: ignoreversion
; Nessun config.ini viene distribuito: il subcommand `install` in [Run] lo
; riscrive dai default embedded nel nuovo binario (config.Reset), salvando
; prima il file precedente come config.prev.ini. Le personalizzazioni locali
; NON sopravvivono all'upgrade: ogni release riparte dal proprio default.

[Registry]
; The tray icon (EMLyUpdater.exe tray, internal/tray), started at every
; user's logon. EMLyUpdater.exe is a console program: run directly from Run
; it would open a console window for the life of the tray, so it is hosted by
; a headless conhost instead (Windows 10 1809+). The value name is the
; display name Task Manager's Startup tab shows.
Root: HKLM; Subkey: "Software\Microsoft\Windows\CurrentVersion\Run"; ValueType: string; ValueName: "{#ProductName}"; ValueData: """{sys}\conhost.exe"" --headless ""{app}\{#ApplicationName}.exe"" tray"; Flags: uninsdeletevalue

[Run]
; Register (or refresh, on upgrade) the auto-start LocalSystem service, the
; Event Log source, and the ProgramData tree; then start it.
Filename: "{app}\{#ApplicationName}.exe"; Parameters: "install"; Flags: runhidden waituntilterminated
Filename: "{app}\{#ApplicationName}.exe"; Parameters: "start"; Flags: runhidden waituntilterminated
; Interactive installs only: start the tray now for the user who ran the
; setup rather than at their next logon. A silent install (GPO, self-update)
; skips it - there the Restart Manager relaunches a tray it closed to replace
; the exe (RegisterApplicationRestart), and every other session gets it at
; logon.
Filename: "{sys}\conhost.exe"; Parameters: "--headless ""{app}\{#ApplicationName}.exe"" tray"; Flags: nowait runasoriginaluser skipifsilent

[UninstallRun]
; Stops and deletes the service and removes the Event Log source.
; C:\ProgramData\EMLyUpdater (config, state, logs) is deliberately kept.
Filename: "{app}\{#ApplicationName}.exe"; Parameters: "uninstall"; Flags: runhidden waituntilterminated; RunOnceId: "RemoveService"

[Code]
// On upgrades the service holds a lock on EMLyUpdater.exe; stop it before the
// [Files] phase. The `stop` subcommand waits until the SCM reports Stopped
// (up to 60s), so no extra polling is needed here. A fresh install has no
// previous exe and skips this.
function PrepareToInstall(var NeedsRestart: Boolean): String;
var
  OldExe: String;
  ResultCode: Integer;
begin
  Result := '';
  OldExe := ExpandConstant('{app}\{#ApplicationName}.exe');
  if FileExists(OldExe) then
    Exec(OldExe, 'stop', '', SW_HIDE, ewWaitUntilTerminated, ResultCode);
end;

// The "wingetmodule" component's downloads (the module, PowerShell 7) run
// on a download page with a progress bar, right after "Ready to Install" -
// not from ssPostInstall, where they would sit behind a frozen "Finishing
// installation" for as long as ~120 MB takes to arrive. It also keeps them
// ahead of PrepareToInstall, so the service is not left stopped while they
// download. In a silent install the page is invisible but Setup still
// simulates the Next click on wpReady, so the same code path runs.
//
// Best-effort by design: this runs from [Code] rather than as [Files]
// "download" entries because a failed [Files] download is a setup error, and
// under /VERYSILENT /SUPPRESSMSGBOXES that aborts the whole install - an
// unreachable PowerShell Gallery or GitHub would then block the updater's own
// upgrade, self-update included. Here every failure (network, SHA256
// mismatch, the user pressing Abort) is logged (/LOG) and the updater installs
// anyway, without that component. Each file is downloaded on its own so one
// failing does not cost the other.
var
  DownloadPage: TDownloadWizardPage;
  WinGetModuleArchiveReady, PwshMsiReady: Boolean;

function WinGetModuleDir: String;
begin
  Result := ExpandConstant('{commonpf64}\WindowsPowerShell\Modules\{#WinGetModuleName}\{#WinGetModuleVersion}');
end;

// HasModuleVersionAtLeast reports whether Root\<version>\<module>.psd1 exists
// for a version folder >= the pinned one. An older one does not count: the
// pinned version is then installed beside it, and internal/winget picks the
// highest version folder.
function HasModuleVersionAtLeast(const Root: String): Boolean;
var
  FindRec: TFindRec;
  Found, Pinned: Int64;
begin
  Result := False;
  if not StrToVersion('{#WinGetModuleVersion}', Pinned) then
    Exit;
  if not FindFirst(Root + '\*', FindRec) then
    Exit;
  try
    repeat
      if (FindRec.Attributes and FILE_ATTRIBUTE_DIRECTORY <> 0) and
         StrToVersion(FindRec.Name, Found) and
         (ComparePackedVersion(Found, Pinned) >= 0) and
         FileExists(Root + '\' + FindRec.Name + '\{#WinGetModuleName}.psd1') then begin
        Result := True;
        Exit;
      end;
    until not FindNext(FindRec);
  finally
    FindClose(FindRec);
  end;
end;

// WinGetModuleInstalled looks only where the SYSTEM service can load the
// module from: the AllUsers module paths of Windows PowerShell and of
// PowerShell 7 (both are on pwsh's PSModulePath). A CurrentUser install
// (Documents\...\Modules) is invisible to SYSTEM, so it does not count.
function WinGetModuleInstalled: Boolean;
begin
  Result :=
    HasModuleVersionAtLeast(ExpandConstant('{commonpf64}\WindowsPowerShell\Modules\{#WinGetModuleName}')) or
    HasModuleVersionAtLeast(ExpandConstant('{commonpf64}\PowerShell\Modules\{#WinGetModuleName}'));
end;

// PwshInstalled checks where internal/winget looks for pwsh.exe first. Any
// version counts (see InstallPwsh).
function PwshInstalled: Boolean;
begin
  Result := FileExists(ExpandConstant('{commonpf64}\PowerShell\7\pwsh.exe'));
end;

function WinGetModuleNeeded: Boolean;
begin
  Result := WizardIsComponentSelected('wingetmodule') and not WinGetModuleInstalled;
end;

function PwshNeeded: Boolean;
begin
  Result := WizardIsComponentSelected('wingetmodule') and not PwshInstalled;
end;

// DownloadOne fetches Url into {tmp}\BaseName through the download page,
// verifying Sha256. False, logged, on any failure.
function DownloadOne(const What, Url, BaseName, Sha256: String): Boolean;
begin
  Result := False;
  DownloadPage.Clear;
  DownloadPage.Add(Url, BaseName, Sha256);
  try
    Log(What + ': downloading ' + Url);
    DownloadPage.Download;
    Result := True;
  except
    Log(What + ': download failed, it will NOT be installed: ' + GetExceptionMessage);
  end;
end;

procedure InitializeWizard;
begin
  DownloadPage := CreateDownloadPage(SetupMessage(msgWizardPreparing), SetupMessage(msgPreparingDesc), nil);
end;

// When PowerShell 7 and the module are both already on the machine the
// "wingetmodule" component has nothing left to do, so on an interactive run
// its checkbox is greyed out and says so. Its checked state is left as the
// type/previous install set it, so the choice recorded for upgrades does not
// change. With only one of the two present it stays selectable and installs
// just the missing one. Silent installs never show this page: there,
// /COMPONENTS="...,wingetmodule" is accepted as is and PwshNeeded /
// WinGetModuleNeeded skip both downloads.
procedure CurPageChanged(CurPageID: Integer);
var
  I: Integer;
begin
  if (CurPageID <> wpSelectComponents) or not (PwshInstalled and WinGetModuleInstalled) then
    Exit;
  // Matched on the caption from [Components], since the list has no names.
  for I := 0 to WizardForm.ComponentsList.Items.Count - 1 do
    if Pos('{#WinGetModuleName}', WizardForm.ComponentsList.ItemCaption[I]) = 1 then begin
      WizardForm.ComponentsList.ItemEnabled[I] := False;
      WizardForm.ComponentsList.ItemSubItem[I] := 'already installed';
    end;
end;

function NextButtonClick(CurPageID: Integer): Boolean;
begin
  Result := True;
  if (CurPageID <> wpReady) or not (PwshNeeded or WinGetModuleNeeded) then
    Exit;
  DownloadPage.Show;
  try
    if PwshNeeded then
      PwshMsiReady := DownloadOne('PowerShell 7', '{#PwshURL}', '{#PwshMSI}', '{#PwshSHA256}');
    if WinGetModuleNeeded then
      WinGetModuleArchiveReady := DownloadOne('WinGet module', '{#WinGetModuleURL}', '{#WinGetModuleName}.zip', '{#WinGetModuleSHA256}');
  finally
    DownloadPage.Hide;
  end;
end;

// Installs Microsoft.WinGet.Client for all users of Windows PowerShell 5.1
// (the equivalent of Install-Module -Scope AllUsers), where the SYSTEM
// service can load it too, from the archive NextButtonClick downloaded.
//
// Uninstall deliberately leaves the module in place: another product or an
// administrator may rely on the same version.
procedure InstallWinGetModule;
var
  Dest, Staging, Archive: String;
begin
  if not WizardIsComponentSelected('wingetmodule') then
    Exit;

  if WinGetModuleInstalled then begin
    Log('WinGet module: {#WinGetModuleVersion} or newer already installed, not downloaded');
    Exit;
  end;
  Dest := WinGetModuleDir;
  if not WinGetModuleArchiveReady then begin
    Log('WinGet module: NOT installed, the updater is installed without it: nothing was downloaded');
    Exit;
  end;

  // Extract into a staging directory beside the destination and rename it
  // into place only once complete: the .psd1 check above must never find a
  // half-extracted module left behind by an interrupted install.
  Staging := Dest + '.partial';
  try
    WizardForm.StatusLabel.Caption := 'Installing {#WinGetModuleName} {#WinGetModuleVersion}...';
    Archive := ExpandConstant('{tmp}\{#WinGetModuleName}.zip');

    if DirExists(Staging) then
      DelTree(Staging, True, True, True);
    if not ForceDirectories(Staging) then
      RaiseException('cannot create ' + Staging);
    ExtractArchive(Archive, Staging, '', True, nil);

    // NuGet packaging metadata, which Install-Module strips too.
    DelTree(Staging + '\_rels', True, True, True);
    DelTree(Staging + '\package', True, True, True);
    DeleteFile(Staging + '\[Content_Types].xml');
    DeleteFile(Staging + '\{#WinGetModuleName}.nuspec');

    if not FileExists(Staging + '\{#WinGetModuleName}.psd1') then
      RaiseException('archive does not contain {#WinGetModuleName}.psd1');
    if DirExists(Dest) then
      DelTree(Dest, True, True, True);
    if not RenameFile(Staging, Dest) then
      RaiseException('cannot move ' + Staging + ' to ' + Dest);
    Log('WinGet module: installed {#WinGetModuleVersion} in ' + Dest);
  except
    Log('WinGet module: NOT installed, the updater is installed without it: ' + GetExceptionMessage);
    if DirExists(Staging) then
      DelTree(Staging, True, True, True);
  end;
end;

// Installs PowerShell 7 machine-wide from its MSI, which is where
// internal/winget looks for pwsh.exe first ({commonpf64}\PowerShell\7).
//
// Any pwsh.exe already there is left alone, whatever its version: it is
// good enough to run the module, it may be managed by another tool
// (Intune, Microsoft Update, winget), and the MSI would refuse a downgrade
// anyway. Installed from the MSI NextButtonClick downloaded, and kept on
// uninstall for the same reason as the module. The MSI's log goes next to the
// updater's own logs so a failed install can be diagnosed on the machine.
procedure InstallPwsh;
var
  Msi, LogFile: String;
  ResultCode: Integer;
begin
  if not WizardIsComponentSelected('wingetmodule') then
    Exit;

  if PwshInstalled then begin
    Log('PowerShell 7: already installed, left alone, not downloaded');
    Exit;
  end;
  if not PwshMsiReady then begin
    Log('PowerShell 7: NOT installed, the updater is installed without it: nothing was downloaded');
    Exit;
  end;

  try
    WizardForm.StatusLabel.Caption := 'Installing PowerShell {#PwshVersion}...';
    Msi := ExpandConstant('{tmp}\{#PwshMSI}');
    LogFile := ExpandConstant('{commonappdata}\{#ApplicationName}\logs\pwsh-install-{#PwshVersion}.log');
    ForceDirectories(ExtractFileDir(LogFile));

    if not Exec(ExpandConstant('{sys}\msiexec.exe'),
        '/i "' + Msi + '" /qn /norestart ADD_PATH=1 /l*v "' + LogFile + '"',
        '', SW_HIDE, ewWaitUntilTerminated, ResultCode) then
      RaiseException('cannot run msiexec: ' + SysErrorMessage(ResultCode));
    // 3010: installed, a reboot is needed to finish - pwsh.exe already works.
    if (ResultCode <> 0) and (ResultCode <> 3010) then
      RaiseException('msiexec exited with ' + IntToStr(ResultCode) + ', see ' + LogFile);
    Log('PowerShell 7: installed {#PwshVersion}');
  except
    Log('PowerShell 7: NOT installed, the updater is installed without it: ' + GetExceptionMessage);
  end;
end;

procedure CurStepChanged(CurStep: TSetupStep);
begin
  if CurStep = ssPostInstall then begin
    InstallPwsh;
    InstallWinGetModule;
  end;
end;

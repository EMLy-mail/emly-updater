#define ApplicationName 'EMLyUpdater'
#define ApplicationVersion '1.7.2'
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

[Setup]
AppName={#ApplicationName}
AppVersion={#ApplicationVersion}
AppVerName={#ApplicationName} {#ApplicationVersion}
DefaultDirName={autopf}\{#ApplicationName}
OutputBaseFilename={#ApplicationName}_Installer_{#ApplicationVersion}
ArchitecturesInstallIn64BitMode=x64compatible
DisableProgramGroupPage=yes
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
; /COMPONENTS replaces the selection, so list every component wanted. The
; updater's own [Files] carry no Components: parameter and are installed
; whatever the selection. Without /COMPONENTS an upgrade (self-update
; included) keeps the previous install's choice.
[Types]
Name: "compact"; Description: "EMLy Updater only"
Name: "full"; Description: "EMLy Updater + WinGet PowerShell module"
Name: "custom"; Description: "Custom"; Flags: iscustom

[Components]
Name: "updater"; Description: "EMLy Updater service"; Types: compact full custom; Flags: fixed
; ExtraDiskSpaceRequired: the module's extracted size, since nothing in
; [Files] accounts for it.
Name: "wingetmodule"; Description: "{#WinGetModuleName} {#WinGetModuleVersion} PowerShell module (downloaded from PowerShell Gallery)"; Types: full; ExtraDiskSpaceRequired: 56025934

[Files]
; Built by: go build -ldflags "-s -w" -o build\EMLyUpdater.exe .
Source: "..\build\{#ApplicationName}.exe"; DestDir: "{app}"; Flags: ignoreversion
Source: "appicon.ico"; DestDir: "{app}"; Flags: ignoreversion
; Nessun config.ini viene distribuito: il subcommand `install` in [Run] lo
; riscrive dai default embedded nel nuovo binario (config.Reset), salvando
; prima il file precedente come config.prev.ini. Le personalizzazioni locali
; NON sopravvivono all'upgrade: ogni release riparte dal proprio default.

[Run]
; Register (or refresh, on upgrade) the auto-start LocalSystem service, the
; Event Log source, and the ProgramData tree; then start it.
Filename: "{app}\{#ApplicationName}.exe"; Parameters: "install"; Flags: runhidden waituntilterminated
Filename: "{app}\{#ApplicationName}.exe"; Parameters: "start"; Flags: runhidden waituntilterminated

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

// Installs Microsoft.WinGet.Client for all users of Windows PowerShell 5.1
// (the equivalent of Install-Module -Scope AllUsers), where the SYSTEM
// service can load it too.
//
// Best-effort by design: this runs from [Code] rather than as a [Files]
// "download" entry because a failed [Files] download is a setup error, and
// under /VERYSILENT /SUPPRESSMSGBOXES that aborts the whole install - an
// unreachable PowerShell Gallery would then block the updater's own upgrade,
// self-update included. Here every failure is logged (/LOG) and the updater
// installs anyway, without the module.
//
// Uninstall deliberately leaves the module in place: another product or an
// administrator may rely on the same version.
procedure InstallWinGetModule;
var
  ModulesDir, Dest, Staging, Archive: String;
begin
  if not WizardIsComponentSelected('wingetmodule') then
    Exit;

  ModulesDir := ExpandConstant('{commonpf64}\WindowsPowerShell\Modules\{#WinGetModuleName}');
  Dest := ModulesDir + '\{#WinGetModuleVersion}';
  if FileExists(Dest + '\{#WinGetModuleName}.psd1') then begin
    Log('WinGet module: {#WinGetModuleVersion} already installed in ' + Dest);
    Exit;
  end;

  // Extract into a staging directory beside the destination and rename it
  // into place only once complete: the .psd1 check above must never find a
  // half-extracted module left behind by an interrupted install.
  Staging := Dest + '.partial';
  try
    Log('WinGet module: downloading {#WinGetModuleURL}');
    // Raises on a network error or a SHA256 mismatch.
    DownloadTemporaryFile('{#WinGetModuleURL}', '{#WinGetModuleName}.zip', '{#WinGetModuleSHA256}', nil);
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

procedure CurStepChanged(CurStep: TSetupStep);
begin
  if CurStep = ssPostInstall then
    InstallWinGetModule;
end;

#define ApplicationName 'EMLyUpdater'
#define ApplicationVersion '1.1.0b'
#define ServiceName 'EMLyUpdater'

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

[Files]
; Built by: go build -ldflags "-s -w" -o build\EMLyUpdater.exe .
Source: "..\build\{#ApplicationName}.exe"; DestDir: "{app}"; Flags: ignoreversion
Source: "appicon.ico"; DestDir: "{app}"; Flags: ignoreversion
; Nessun config.ini viene distribuito: il subcommand `install` in [Run]
; lo scrive sempre da zero dai default embedded nel binario (il vecchio
; config viene rimosso in [InstallDelete] prima dell'esecuzione di [Run]).

[InstallDelete]
; Rimuove il config precedente prima di [Run], così il subcommand `install`
; lo riscrive sempre dai default embedded nel nuovo binario.
Type: files; Name: "{commonappdata}\EMLyUpdater\config.ini"

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

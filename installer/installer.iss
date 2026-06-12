#define ApplicationName 'EMLyUpdater'
#define ApplicationVersion '0.2.1'
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
UninstallDisplayIcon={app}\{#ApplicationName}.exe
WizardStyle=modern

[Files]
; Built by: go build -ldflags "-s -w" -o build\EMLyUpdater.exe .
Source: "..\build\{#ApplicationName}.exe"; DestDir: "{app}"; Flags: ignoreversion
; No config file is shipped: the `install` subcommand below seeds
; C:\ProgramData\EMLyUpdater\config.ini from defaults embedded in the binary,
; only when the file does not exist yet - per-machine tweaks survive upgrades
; and the updater's uninstall (requirement 8).

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

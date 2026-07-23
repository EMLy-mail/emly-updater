package notify

import (
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

// UpdateCompleteMessage builds the localized title/body announcing a
// successful EMLy update, e.g. "EMLy has been updated to version 1.4.2
// (beta)." "stable"/"beta" are the two values EMLy's GUI_RELEASE_CHANNEL can
// hold; "stable" is localized, "beta" is used verbatim in both languages.
func UpdateCompleteMessage(lang, version, channel string) Message {
	if lang == "it" {
		label := channel
		if channel == "stable" {
			label = "stabile"
		}
		return Message{
			Title: "EMLy aggiornato",
			Body:  fmt.Sprintf("EMLy è stato aggiornato alla versione %s (%s).", version, label),
		}
	}
	return Message{
		Title: "EMLy Updated",
		Body:  fmt.Sprintf("EMLy has been updated to version %s (%s).", version, channel),
	}
}

// LaunchToast shows the update-complete toast in the active console user's
// session. Session 0, where this SYSTEM service lives, has no desktop to
// draw a notification-area icon on - so this re-launches updaterExePath as
// the console user (WTSQueryUserToken + CreateProcessAsUser, the standard
// SYSTEM-service -> interactive-session hop) with the "show-toast"
// subcommand, whose handler (internal/toast.Show) does the actual drawing
// inside that session.
//
// Returns false (skipped, not an error) when there is no active console
// session - nobody is there to see it - mirroring WarnCriticalUpdate. Every
// failure path is best-effort: a toast is a courtesy notification, never
// something the update itself should fail over.
func LaunchToast(updaterExePath, emlyExePath, title, body string) bool {
	session, _, _ := procActiveConsole.Call()
	if uint32(session) == noConsoleSession {
		return false
	}

	// LocalSystem holds these privileges but they may not be enabled in the
	// service's own token; WTSQueryUserToken in particular requires
	// SeTcbPrivilege to succeed.
	for _, priv := range []string{"SeTcbPrivilege", "SeAssignPrimaryTokenPrivilege", "SeIncreaseQuotaPrivilege"} {
		_ = enablePrivilege(priv) // best-effort
	}

	var userToken windows.Token
	if err := windows.WTSQueryUserToken(uint32(session), &userToken); err != nil {
		return false
	}
	defer userToken.Close()

	var primaryToken windows.Token
	if err := windows.DuplicateTokenEx(userToken, windows.MAXIMUM_ALLOWED, nil,
		windows.SecurityImpersonation, windows.TokenPrimary, &primaryToken); err != nil {
		return false
	}
	defer primaryToken.Close()

	var envBlock *uint16
	if err := windows.CreateEnvironmentBlock(&envBlock, primaryToken, false); err != nil {
		return false
	}
	defer windows.DestroyEnvironmentBlock(envBlock)

	cmdLine := windows.ComposeCommandLine([]string{
		updaterExePath, "show-toast",
		"--exe", emlyExePath,
		"--title", title,
		"--body", body,
	})
	cmdLineU16, err := windows.UTF16PtrFromString(cmdLine)
	if err != nil {
		return false
	}

	desktop, err := windows.UTF16PtrFromString(`winsta0\default`)
	if err != nil {
		return false
	}

	var si windows.StartupInfo
	si.Cb = uint32(unsafe.Sizeof(si))
	si.Desktop = desktop

	var pi windows.ProcessInformation
	if err := windows.CreateProcessAsUser(
		primaryToken, nil, cmdLineU16, nil, nil, false,
		windows.CREATE_UNICODE_ENVIRONMENT|windows.CREATE_NO_WINDOW,
		envBlock, nil, &si, &pi,
	); err != nil {
		return false
	}
	windows.CloseHandle(pi.Thread)
	windows.CloseHandle(pi.Process)
	return true
}

// enablePrivilege enables name in the current process's own token (it is
// commonly present but disabled by default even for LocalSystem).
func enablePrivilege(name string) error {
	var procToken windows.Token
	if err := windows.OpenProcessToken(windows.CurrentProcess(),
		windows.TOKEN_ADJUST_PRIVILEGES|windows.TOKEN_QUERY, &procToken); err != nil {
		return fmt.Errorf("OpenProcessToken: %w", err)
	}
	defer procToken.Close()

	namePtr, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return err
	}
	var luid windows.LUID
	if err := windows.LookupPrivilegeValue(nil, namePtr, &luid); err != nil {
		return fmt.Errorf("LookupPrivilegeValue(%s): %w", name, err)
	}

	tp := windows.Tokenprivileges{
		PrivilegeCount: 1,
		Privileges: [1]windows.LUIDAndAttributes{
			{Luid: luid, Attributes: windows.SE_PRIVILEGE_ENABLED},
		},
	}
	if err := windows.AdjustTokenPrivileges(procToken, false, &tp, 0, nil, nil); err != nil {
		return fmt.Errorf("AdjustTokenPrivileges(%s): %w", name, err)
	}
	return nil
}

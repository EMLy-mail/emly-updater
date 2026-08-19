package notify

import "golang.org/x/sys/windows"

// ConsoleUserSID returns the string SID of the user logged on at the active
// console session, and whether there is one at all.
//
// ok == false is a normal state, not an error: a machine sitting at the login
// screen has no console user. Callers should carry on with whatever part of
// their work is machine-wide.
//
// This exists because a LocalSystem service cannot reach the interactive
// user's per-profile state through its own token - that token resolves to
// SYSTEM's profile. Anything per-user (a certificate store, a registry hive)
// has to be addressed by the interactive user's SID, which is what this
// returns.
func ConsoleUserSID() (string, bool) {
	session, _, _ := procActiveConsole.Call()
	if uint32(session) == noConsoleSession {
		return "", false
	}

	// WTSQueryUserToken requires SeTcbPrivilege. LocalSystem holds it, but it
	// is not necessarily enabled in the service's own token - the same
	// best-effort enable LaunchToast does.
	_ = enablePrivilege("SeTcbPrivilege")

	var token windows.Token
	if err := windows.WTSQueryUserToken(uint32(session), &token); err != nil {
		return "", false
	}
	defer token.Close()

	user, err := token.GetTokenUser()
	if err != nil {
		return "", false
	}
	return user.User.Sid.String(), true
}

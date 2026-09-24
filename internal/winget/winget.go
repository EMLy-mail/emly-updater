// Package winget lists the Windows packages winget can upgrade, through the
// Microsoft.WinGet.Client PowerShell module. It is read-only: nothing here
// ever installs or upgrades anything.
//
// The text output of `winget upgrade` is deliberately not parsed: it is
// localized, column-truncated and changes between winget releases. The
// module's objects, serialized with ConvertTo-Json, are a stable contract.
package winget

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// Package is one installed package with an update available.
type Package struct {
	Name             string `json:"Name"`
	ID               string `json:"Id"`
	InstalledVersion string `json:"InstalledVersion"`
	Available        string `json:"Available"`
	Source           string `json:"Source"`
}

// Script is the PowerShell run by ListUpgradable. @(...) with -InputObject
// keeps the output an array even for zero or one package (piping into
// ConvertTo-Json would unwrap a single element into a bare object).
//
// $ErrorActionPreference = 'Stop' is what makes a missing module fail the
// run: without it Get-WinGetPackage's CommandNotFoundException is a
// non-terminating error, ConvertTo-Json still prints "[]" and the process
// exits 0 - indistinguishable from "nothing to update".
// $ProgressPreference keeps the module's progress bar out of the output.
const Script = `$ErrorActionPreference = 'Stop';
$ProgressPreference = 'SilentlyContinue';
[Console]::OutputEncoding = [System.Text.Encoding]::UTF8;
ConvertTo-Json -InputObject @(Get-WinGetPackage | Where-Object IsUpdateAvailable | Select-Object Name, Id, InstalledVersion, @{n='Available';e={$_.AvailableVersions[0]}}, Source)`

// Runner executes a PowerShell script and returns its stdout. A non-zero
// exit should be reported as an *ExitError so its stderr reaches the caller.
// Tests replace it to feed canned output without running PowerShell.
type Runner func(ctx context.Context, script string) ([]byte, error)

// ExitError is a PowerShell run that exited with a non-zero code.
type ExitError struct {
	Code   int
	Stderr string
}

func (e *ExitError) Error() string {
	return fmt.Sprintf("powershell exited with code %d: %s", e.Code, strings.TrimSpace(e.Stderr))
}

var (
	// ErrPowerShellNotFound means powershell.exe is not on PATH.
	ErrPowerShellNotFound = errors.New("powershell.exe not found in PATH")
	// ErrModuleNotInstalled means the Microsoft.WinGet.Client module is missing.
	ErrModuleNotInstalled = errors.New(`PowerShell module Microsoft.WinGet.Client is not installed; install it with: Install-Module Microsoft.WinGet.Client -Scope CurrentUser, or re-run the EMLyUpdater setup with /COMPONENTS="updater,wingetmodule"`)
)

// PowerShell is the default Runner: powershell.exe with no profile, no
// prompts and the execution policy bypassed for this process only.
func PowerShell(ctx context.Context, script string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "powershell.exe",
		"-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-Command", script)
	cmd.Env = withoutPSModulePath(os.Environ())
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	// A killed powershell.exe can leave a child holding the pipes open;
	// without WaitDelay, Wait would outlive the context's deadline.
	cmd.WaitDelay = 5 * time.Second

	err := cmd.Run()
	if err == nil {
		return stdout.Bytes(), nil
	}
	if errors.Is(err, exec.ErrNotFound) {
		return nil, ErrPowerShellNotFound
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, fmt.Errorf("powershell did not finish in time: %w", ctxErr)
	}
	if exitErr, ok := errors.AsType[*exec.ExitError](err); ok {
		return stdout.Bytes(), &ExitError{Code: exitErr.ExitCode(), Stderr: stderr.String()}
	}
	return nil, fmt.Errorf("running powershell: %w", err)
}

// withoutPSModulePath drops PSModulePath from env. Launched from a
// PowerShell 7 session, this process inherits pwsh's module path, which lacks
// Windows PowerShell's per-user Documents\WindowsPowerShell\Modules - where
// `Install-Module -Scope CurrentUser` puts Microsoft.WinGet.Client - and
// powershell.exe then reports Get-WinGetPackage as not recognized. pwsh itself
// resets the variable when it starts powershell.exe; a Go parent does not.
// Unset, powershell.exe computes its own default (user, Program Files, system).
func withoutPSModulePath(env []string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		name, _, _ := strings.Cut(kv, "=")
		if !strings.EqualFold(name, "PSModulePath") {
			out = append(out, kv)
		}
	}
	return out
}

// ListUpgradable returns the packages with an update available, using the
// real PowerShell.
func ListUpgradable(ctx context.Context) ([]Package, error) {
	return ListUpgradableWith(ctx, PowerShell)
}

// ListUpgradableWith is ListUpgradable with the Runner injected.
func ListUpgradableWith(ctx context.Context, run Runner) ([]Package, error) {
	out, err := run(ctx, Script)
	if err != nil {
		if exitErr, ok := errors.AsType[*ExitError](err); ok && isModuleMissing(exitErr.Stderr) {
			return nil, ErrModuleNotInstalled
		}
		return nil, err
	}
	return Parse(out)
}

// isModuleMissing recognizes the stderr of a run where Get-WinGetPackage
// could not be resolved: English CommandNotFoundException text or its
// fully-qualified error id, which PowerShell does not localize.
func isModuleMissing(stderr string) bool {
	if !strings.Contains(stderr, "Get-WinGetPackage") {
		return false
	}
	return strings.Contains(stderr, "CommandNotFoundException") ||
		strings.Contains(stderr, "is not recognized") ||
		strings.Contains(stderr, "Microsoft.WinGet.Client")
}

// Parse decodes ConvertTo-Json output into packages. Empty output and "[]"
// both mean no updates; a UTF-8 BOM is tolerated, and so is a bare object in
// case a PowerShell version ever unwraps a one-element array.
func Parse(out []byte) ([]Package, error) {
	data := bytes.TrimSpace(bytes.TrimPrefix(out, []byte("\xEF\xBB\xBF")))
	if len(data) == 0 {
		return []Package{}, nil
	}

	if data[0] == '{' {
		var p Package
		if err := json.Unmarshal(data, &p); err != nil {
			return nil, invalidJSON(err, data)
		}
		return []Package{p}, nil
	}

	pkgs := []Package{}
	if err := json.Unmarshal(data, &pkgs); err != nil {
		return nil, invalidJSON(err, data)
	}
	return pkgs, nil
}

func invalidJSON(err error, data []byte) error {
	const maxExcerpt = 200
	excerpt := string(data)
	if len(excerpt) > maxExcerpt {
		excerpt = excerpt[:maxExcerpt] + "..."
	}
	return fmt.Errorf("invalid JSON from powershell: %w; output starts with: %q", err, excerpt)
}

// FilterBySource keeps the packages whose Source matches source,
// case-insensitively. An empty source returns pkgs unchanged.
func FilterBySource(pkgs []Package, source string) []Package {
	if source == "" {
		return pkgs
	}
	out := []Package{}
	for _, p := range pkgs {
		if strings.EqualFold(p.Source, source) {
			out = append(out, p)
		}
	}
	return out
}

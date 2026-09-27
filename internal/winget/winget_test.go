package winget

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

var (
	git = Package{Name: "Git", ID: "Git.Git", InstalledVersion: "2.44.0", Available: "2.46.0", Source: "winget"}
	vsc = Package{Name: "Visual Studio Code", ID: "Microsoft.VisualStudioCode", InstalledVersion: "1.90.0", Available: "1.92.1", Source: "winget"}
	app = Package{Name: "Store App", ID: "9NBLGGH4NNS1", InstalledVersion: "1.0", Available: "1.1", Source: "msstore"}
)

const (
	gitJSON = `{"Name":"Git","Id":"Git.Git","InstalledVersion":"2.44.0","Available":"2.46.0","Source":"winget"}`
	vscJSON = `{"Name":"Visual Studio Code","Id":"Microsoft.VisualStudioCode","InstalledVersion":"1.90.0","Available":"1.92.1","Source":"winget"}`
)

func TestParse(t *testing.T) {
	tests := []struct {
		name    string
		out     string
		want    []Package
		wantErr string
	}{
		{name: "empty array", out: "[]\r\n", want: []Package{}},
		{name: "single element", out: "[\r\n" + gitJSON + "\r\n]", want: []Package{git}},
		{name: "multiple elements", out: "[" + gitJSON + "," + vscJSON + "]", want: []Package{git, vsc}},
		{name: "leading BOM", out: "\xEF\xBB\xBF[" + gitJSON + "]", want: []Package{git}},
		{name: "bare object", out: gitJSON, want: []Package{git}},
		{name: "empty output", out: "", want: []Package{}},
		{name: "whitespace only", out: " \r\n", want: []Package{}},
		{name: "invalid JSON", out: "[{\"Name\": oops", wantErr: `output starts with: "[{\"Name\": oops"`},
		{name: "not JSON at all", out: "WARNING: something", wantErr: "invalid JSON"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Parse([]byte(tt.out))
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("got %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestParseTruncatesExcerpt(t *testing.T) {
	_, err := Parse([]byte("[" + strings.Repeat("x", 1000)))
	if err == nil || len(err.Error()) > 400 {
		t.Fatalf("expected a short error, got %d chars: %v", len(err.Error()), err)
	}
}

func TestListUpgradableWith(t *testing.T) {
	boom := errors.New("boom")
	tests := []struct {
		name    string
		out     string
		err     error
		want    []Package
		wantIs  error
		wantErr string
	}{
		{name: "success", out: "[" + gitJSON + "]", want: []Package{git}},
		{
			name: "module missing",
			err: &ExitError{Code: 1, Stderr: "Get-WinGetPackage : The term 'Get-WinGetPackage' is not recognized as the name of a cmdlet\r\n" +
				"    + FullyQualifiedErrorId : CommandNotFoundException"},
			wantIs: ErrModuleNotInstalled,
		},
		{
			// A runtime failure of a module that did load:
			// the error id names Microsoft.WinGet.Client, but it is not a
			// missing module and must reach the caller as-is.
			name: "cmdlet error is not a missing module",
			err: &ExitError{Code: 1, Stderr: "Get-WinGetPackage : No source matches the given value: winget\r\n" +
				"    + CategoryInfo          : NotSpecified: (:) [Get-WinGetPackage], InvalidSourceException\r\n" +
				"    + FullyQualifiedErrorId : Microsoft.WinGet.Client.Engine.Exceptions.InvalidSourceException,Microsoft.WinGet.Client.Commands.GetPackageCmdlet"},
			wantErr: "InvalidSourceException",
		},
		{
			// Exactly what the service logged when run as SYSTEM under
			// Windows PowerShell 5.1.
			name: "windows powershell refused",
			err: &ExitError{Code: 1, Stderr: "Get-WinGetPackage : This cmdlet is not supported in Windows PowerShell.\r\n" +
				"    + CategoryInfo          : NotSpecified: (:) [Get-WinGetPackage], WindowsPowerShellNotSupported\r\n" +
				"    + FullyQualifiedErrorId : RuntimeException,Microsoft.WinGet.Client.Commands.GetPackageCmdlet"},
			wantIs: ErrPowerShell7Required,
		},
		{name: "other non-zero exit", err: &ExitError{Code: 1, Stderr: "Access denied"}, wantErr: "code 1: Access denied"},
		{name: "powershell missing", err: ErrPowerShellNotFound, wantIs: ErrPowerShellNotFound},
		{name: "runner error passes through", err: boom, wantIs: boom},
		{name: "invalid JSON", out: "nope", wantErr: "invalid JSON"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var gotScript string
			run := func(_ context.Context, script string) ([]byte, error) {
				gotScript = script
				return []byte(tt.out), tt.err
			}
			got, err := ListUpgradableWith(context.Background(), run)
			if gotScript != Script {
				t.Fatalf("runner got a different script")
			}
			switch {
			case tt.wantIs != nil:
				if !errors.Is(err, tt.wantIs) {
					t.Fatalf("err = %v, want %v", err, tt.wantIs)
				}
			case tt.wantErr != "":
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want containing %q", err, tt.wantErr)
				}
			default:
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if !reflect.DeepEqual(got, tt.want) {
					t.Fatalf("got %+v, want %+v", got, tt.want)
				}
			}
		})
	}
}

func TestWithoutPSModulePath(t *testing.T) {
	got := withoutPSModulePath([]string{"PATH=C:\\x", "PSModulePath=C:\\pwsh", "psmodulepath=y", "PSModulePathX=keep"})
	want := []string{"PATH=C:\\x", "PSModulePathX=keep"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestFilterBySource(t *testing.T) {
	all := []Package{git, app, vsc}
	tests := []struct {
		source string
		want   []Package
	}{
		{"", all},
		{"winget", []Package{git, vsc}},
		{"MSStore", []Package{app}},
		{"nope", []Package{}},
	}
	for _, tt := range tests {
		if got := FilterBySource(all, tt.source); !reflect.DeepEqual(got, tt.want) {
			t.Errorf("FilterBySource(%q) = %+v, want %+v", tt.source, got, tt.want)
		}
	}
}

// Command genversion propagates the version string from versioninfo.json
// (the single source of truth) to every other place in the repo that needs
// it, so it is never edited by hand more than once:
//
//   - internal/version/version_generated.go: the Version Go const
//   - installer/installer.iss: the ApplicationVersion Inno Setup #define
//
// internal/config/config.default.ini used to be patched here too, but its
// userAgent now carries a {{VERSION}} placeholder that config.BuildUserAgent
// resolves at runtime, so the shipped default never needs a version stamp.
//
// Run via "go generate ./..." from the repo root after bumping
// versioninfo.json.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
)

const (
	versionInfoPath  = "versioninfo.json"
	versionGoPath    = "internal/version/version_generated.go"
	installerISSPath = "installer/installer.iss"
)

type versionInfo struct {
	StringFileInfo struct {
		FileVersion string `json:"FileVersion"`
	} `json:"StringFileInfo"`
}

func main() {
	data, err := os.ReadFile(versionInfoPath)
	if err != nil {
		fatal(err)
	}

	var vi versionInfo
	if err := json.Unmarshal(data, &vi); err != nil {
		fatal(fmt.Errorf("parsing %s: %w", versionInfoPath, err))
	}
	v := vi.StringFileInfo.FileVersion
	if v == "" {
		fatal(fmt.Errorf("%s: StringFileInfo.FileVersion is empty", versionInfoPath))
	}

	writeGoConst(v)
	patchInstallerISS(v)
}

func writeGoConst(v string) {
	out := fmt.Sprintf(`// Code generated from versioninfo.json by "go generate"; DO NOT EDIT.
// Run "go generate ./..." after bumping versioninfo.json.

package version

// Version is this build's own semver. versioninfo.json is the single
// source of truth; this file is regenerated from it.
const Version = %q
`, v)

	if err := os.WriteFile(versionGoPath, []byte(out), 0644); err != nil {
		fatal(err)
	}
	fmt.Printf("wrote %s (Version = %q)\n", versionGoPath, v)
}

var issVersionRe = regexp.MustCompile(`(?m)^(#define ApplicationVersion ')[^']*(')(\r?)$`)

func patchInstallerISS(v string) {
	data, err := os.ReadFile(installerISSPath)
	if err != nil {
		fatal(err)
	}
	if !issVersionRe.Match(data) {
		fatal(fmt.Errorf("%s: ApplicationVersion #define not found", installerISSPath))
	}
	patched := issVersionRe.ReplaceAll(data, []byte("${1}"+v+"${2}${3}"))
	if err := os.WriteFile(installerISSPath, patched, 0644); err != nil {
		fatal(err)
	}
	fmt.Printf("patched %s (ApplicationVersion = %q)\n", installerISSPath, v)
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "genversion:", err)
	os.Exit(1)
}

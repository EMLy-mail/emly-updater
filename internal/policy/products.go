package policy

import (
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// ProductSettings is one entry of the document's "products" section.
type ProductSettings struct {
	Enabled           bool             `json:"enabled"`
	Name              string           `json:"name"`
	Channel           string           `json:"channel"`
	InstallDir        string           `json:"installDir"`
	ExeName           string           `json:"exeName"`
	InstallWhenAbsent bool             `json:"installWhenAbsent"`
	Detect            []ProductDetect  `json:"detect"`
	Installer         ProductInstaller `json:"installer"`
}

// ProductDetect is one link of a product's version detection chain.
type ProductDetect struct {
	Type    string `json:"type"`
	Path    string `json:"path"`
	Section string `json:"section"`
	Key     string `json:"key"`
}

// ProductInstaller selects the setup driver.
type ProductInstaller struct {
	Type           string `json:"type"`
	CleanReinstall bool   `json:"cleanReinstall"`
}

// productSlug is the API's slug rule (emly-go-api internal/productreg).
var productSlug = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,19}$`)

// reservedProductSlugs: the API's reserved slugs, plus "emly", which is built
// into the agent and configured elsewhere.
var reservedProductSlugs = []string{"emly", "updater", "all", "manifest", "releases", "download", "products"}

var windowsAbsPath = regexp.MustCompile(`^[A-Za-z]:\\`)

func hasDotDot(path string) bool {
	return slices.Contains(strings.FieldsFunc(path, func(r rune) bool { return r == '\\' || r == '/' }), "..")
}

func validateProducts(ps map[string]ProductSettings, add func(path, msg string)) {
	for _, slug := range slices.Sorted(maps.Keys(ps)) {
		p, base := ps[slug], "/products/"+slug
		if !productSlug.MatchString(slug) {
			add(base, "not a valid product slug (^[a-z0-9][a-z0-9-]{0,19}$)")
			continue
		}
		if slices.Contains(reservedProductSlugs, slug) {
			add(base, "reserved slug (emly is built in and configured elsewhere)")
			continue
		}
		if name := strings.TrimSpace(p.Name); name == "" || len(name) > 64 {
			add(base+"/name", "required, at most 64 characters")
		}
		switch p.Channel {
		case "", "stable", "beta":
		default:
			add(base+"/channel", "must be stable or beta")
		}
		if !windowsAbsPath.MatchString(p.InstallDir) || hasDotDot(p.InstallDir) {
			add(base+"/installDir", `must be an absolute Windows path (X:\...) without ".."`)
		}
		if p.ExeName == "" || strings.ContainsAny(p.ExeName, `\/:`) || !strings.HasSuffix(strings.ToLower(p.ExeName), ".exe") {
			add(base+"/exeName", "must be a file name ending in .exe")
		}
		if len(p.Detect) < 1 || len(p.Detect) > 5 {
			add(base+"/detect", "must list 1 to 5 sources")
		}
		for i, d := range p.Detect {
			dp := base + "/detect/" + strconv.Itoa(i)
			switch d.Type {
			case "ini", "file", "exe":
			default:
				add(dp+"/type", "must be ini, file or exe")
			}
			// ':' covers drive-relative paths ("C:foo", which resolve
			// against that drive's current directory, not installDir) and
			// NTFS alternate data streams ("version.txt:x").
			if d.Path == "" || windowsAbsPath.MatchString(d.Path) || strings.HasPrefix(d.Path, `\`) ||
				strings.HasPrefix(d.Path, "/") || hasDotDot(d.Path) || strings.Contains(d.Path, ":") {
				add(dp+"/path", `must be relative to installDir, without ".." or ':'`)
			}
			if d.Type == "ini" {
				if d.Section == "" {
					add(dp+"/section", "required for ini")
				}
				if d.Key == "" {
					add(dp+"/key", "required for ini")
				}
			}
		}
		switch p.Installer.Type {
		case "nsis", "inno":
		default:
			add(base+"/installer/type", "must be nsis or inno")
		}
	}
}

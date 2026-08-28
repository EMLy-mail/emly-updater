package source

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"emlyupdater/internal/manifest"
)

// HTTPSource serves the manifest from a single HTTP(S) URL and downloads the
// setup from the full URL the manifest provides (stableDownload/betaDownload).
type HTTPSource struct {
	ManifestURL string
	Client      *http.Client
	UserAgent   string // optional; sent as User-Agent header when non-empty
	APIKey      string // optional; sent as X-Api-Key header when non-empty
	Hostname    string // optional; sent as X-EMLy-Hostname header when non-empty
	HWID        string // optional; sent as X-EMLy-HWID header when non-empty
	ADDomain    string // optional; sent as X-EMLy-ADDomain header when non-empty
	InternalIP  string // optional; sent as X-EMLy-IntIP header when non-empty
}

// NewHTTPSource builds an HTTPSource with a sensibly timeouted client.
// The overall request timeout is generous because the setup download (tens of
// MB) goes through the same client; connection establishment is bounded
// separately by the transport defaults.
func NewHTTPSource(manifestURL string) *HTTPSource {
	return &HTTPSource{
		ManifestURL: manifestURL,
		Client:      &http.Client{Timeout: 10 * time.Minute},
	}
}

func (s *HTTPSource) Name() string {
	return fmt.Sprintf("http(%s)", s.ManifestURL)
}

// applyHeaders sets the optional request headers on req.
func (s *HTTPSource) applyHeaders(req *http.Request) {
	if s.UserAgent != "" {
		req.Header.Set("User-Agent", s.UserAgent)
	}
	if s.APIKey != "" {
		req.Header.Set("X-Api-Key", s.APIKey)
	}
	if s.Hostname != "" {
		req.Header.Set("X-EMLy-Hostname", s.Hostname)
	}
	if s.HWID != "" {
		req.Header.Set("X-EMLy-HWID", s.HWID)
	}
	if s.ADDomain != "" {
		req.Header.Set("X-EMLy-ADDomain", s.ADDomain)
	}
	if s.InternalIP != "" {
		req.Header.Set("X-EMLy-IntIP", s.InternalIP)
	}
}

// getJSON fetches a manifest document from url with this source's headers
// applied.
//
// The request is bounded tighter than the shared client timeout: a manifest is
// a few KB and a hung endpoint should fail the attempt fast so the resolver's
// retry/backoff can kick in. The setup download uses the client's own generous
// timeout instead.
func (s *HTTPSource) getJSON(ctx context.Context, url string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("invalid manifest URL %q: %w", url, err)
	}
	s.applyHeaders(req)

	resp, err := s.Client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("manifest request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("%s: %w", url, ErrNotFound)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("manifest endpoint returned HTTP %d", resp.StatusCode)
	}

	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20)) // 4 MB cap, manifests are KB-sized
	if err != nil {
		return nil, fmt.Errorf("failed to read manifest body: %w", err)
	}
	return data, nil
}

func (s *HTTPSource) FetchManifest(ctx context.Context) (*manifest.Manifest, error) {
	data, err := s.getJSON(ctx, s.ManifestURL)
	if err != nil {
		return nil, err
	}
	return manifest.Parse(data)
}

// FetchUpdaterManifest retrieves the updater's own release manifest from url
// (derived from this source's manifest URL by config.UpdaterManifestURL, so it
// is served by the same host that serves EMLy's).
//
// A 404 comes back wrapped in ErrNotFound so the caller can tell "this host
// does not implement the endpoint" - the expected answer from an internal
// mirror that has not been updated yet - from a real failure.
func (s *HTTPSource) FetchUpdaterManifest(ctx context.Context, url string) (*manifest.UpdaterManifest, error) {
	data, err := s.getJSON(ctx, url)
	if err != nil {
		return nil, err
	}
	return manifest.ParseUpdater(data)
}

func (s *HTTPSource) ResolveTarget(m *manifest.Manifest, channel string) (manifest.Target, error) {
	version, downloadURL, err := m.ChannelVersion(channel)
	if err != nil {
		return manifest.Target{}, err
	}
	// API manifests key checksums by version string.
	sha, ok := m.SHA256Checksums[version]
	if !ok || sha == "" {
		return manifest.Target{}, fmt.Errorf("manifest carries no SHA256 checksum for version %s", version)
	}
	return manifest.Target{Version: version, DownloadRef: downloadURL, SHA256: sha}, nil
}

func (s *HTTPSource) FetchSetup(ctx context.Context, t manifest.Target, destPath string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, t.DownloadRef, nil)
	if err != nil {
		return fmt.Errorf("invalid download URL %q: %w", t.DownloadRef, err)
	}
	s.applyHeaders(req)

	resp, err := s.Client.Do(req)
	if err != nil {
		return fmt.Errorf("setup download failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("setup download returned HTTP %d", resp.StatusCode)
	}

	dest, err := os.Create(destPath)
	if err != nil {
		return fmt.Errorf("failed to create %s: %w", destPath, err)
	}
	defer dest.Close()

	if _, err := io.Copy(dest, resp.Body); err != nil {
		return fmt.Errorf("setup download interrupted: %w", err)
	}
	return dest.Close()
}

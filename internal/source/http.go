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

// applyHeaders sets the optional User-Agent and X-Api-Key headers on req.
func (s *HTTPSource) applyHeaders(req *http.Request) {
	if s.UserAgent != "" {
		req.Header.Set("User-Agent", s.UserAgent)
	}
	if s.APIKey != "" {
		req.Header.Set("X-Api-Key", s.APIKey)
	}
}

func (s *HTTPSource) FetchManifest(ctx context.Context) (*manifest.Manifest, error) {
	// Bound the manifest request tighter than the shared client timeout:
	// a manifest is a few KB and a hung endpoint should fail over to UNC fast.
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.ManifestURL, nil)
	if err != nil {
		return nil, fmt.Errorf("invalid manifest URL: %w", err)
	}
	s.applyHeaders(req)

	resp, err := s.Client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("manifest request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("manifest endpoint returned HTTP %d", resp.StatusCode)
	}

	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20)) // 4 MB cap, manifests are KB-sized
	if err != nil {
		return nil, fmt.Errorf("failed to read manifest body: %w", err)
	}
	return manifest.Parse(data)
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

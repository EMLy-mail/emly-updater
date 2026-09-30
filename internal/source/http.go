package source

import (
	"context"
	"errors"
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
	OSVersion   string // optional; sent as X-EMLy-OSVersion header when non-empty
	Serial      string // optional; sent as X-EMLy-Serial header when non-empty
	Product     string // optional; sent as X-EMLy-Product header when non-empty
	// EMLyVersion is the EMLy release installed on this machine, read from
	// EMLy's own config.ini (`[EMLy] GUI_SEMVER`), sent as X-EMLy-AppVersion
	// when non-empty. The header name is a wire contract with the API, which
	// stores it in updater_clients.emly_version; "App" tells it apart from
	// this updater's own version, which travels in the User-Agent. Like LoggedUser it is a snapshot the caller re-reads
	// every time it builds a source, because a setup this updater runs
	// changes it mid-uptime. Empty means EMLy is not installed (or its
	// config is unreadable), which stays unreported rather than being sent
	// as the 0.0.0 fresh-install sentinel.
	EMLyVersion string
	// LoggedUser is the interactive user on this machine (`DOMAIN\user`),
	// sent as X-EMLy-LoggedUser when non-empty. Unlike the machine facts
	// above it is not fixed but a snapshot: the caller re-resolves it every
	// time it builds a source, so the value is at most one poll cycle old.
	// Empty means nobody is logged on - a normal state, not a failure.
	LoggedUser string
	// LoggedUserState says how LoggedUser is attached to the machine
	// (`active-console`, `active-rdp`, `disconnected`), sent as
	// X-EMLy-LoggedUserState when non-empty. Same snapshot as LoggedUser.
	LoggedUserState string
	// LoggedUserDisconnectedAt is when a disconnected session lost its
	// client, sent as X-EMLy-LoggedUserDisconnectedAt (RFC 3339, UTC) when
	// non-zero. Zero for any session that is not disconnected.
	LoggedUserDisconnectedAt time.Time

	// setupIdleTimeout overrides SetupIdleTimeout; tests shrink it. Zero
	// means SetupIdleTimeout.
	setupIdleTimeout time.Duration
}

// NewHTTPSource builds an HTTPSource whose client has no overall timeout.
// Every request bounds itself instead: the manifests (getJSON) and the
// configuration (FetchConfig) with a short context deadline, the setup
// download (FetchSetup) with an inactivity timeout - see SetupIdleTimeout for
// why a total cap would be wrong there.
func NewHTTPSource(manifestURL string) *HTTPSource {
	return &HTTPSource{
		ManifestURL: manifestURL,
		Client:      &http.Client{},
	}
}

// SetupIdleTimeout is how long FetchSetup waits without receiving a single
// byte - to connect, for the response headers, or between body reads - before
// giving the download up as stalled.
//
// It is deliberately not a cap on the whole download. The server bounds a
// download's total duration itself (10 minutes by default, raisable from the
// dashboard up to 24 hours) and a client cap shorter than the server's would
// make the updater truncate the very slow-but-alive downloads a saturated MPLS
// link produces - which is what the old 10-minute http.Client.Timeout did.
// An inactivity timeout never penalises a slow line and still catches a dead
// one.
const SetupIdleTimeout = 2 * time.Minute

// ErrSetupStalled is what FetchSetup returns (wrapped) when its inactivity
// watchdog gives the download up.
var ErrSetupStalled = errors.New("setup download stalled")

// idleReader re-arms a watchdog timer on every read that returns data.
type idleReader struct {
	r     io.Reader
	timer *time.Timer
	idle  time.Duration
}

func (ir *idleReader) Read(p []byte) (int, error) {
	n, err := ir.r.Read(p)
	if n > 0 {
		ir.timer.Reset(ir.idle)
	}
	return n, err
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
	if s.OSVersion != "" {
		req.Header.Set("X-EMLy-OSVersion", s.OSVersion)
	}
	if s.Serial != "" {
		req.Header.Set("X-EMLy-Serial", s.Serial)
	}
	if s.Product != "" {
		req.Header.Set("X-EMLy-Product", s.Product)
	}
	if s.EMLyVersion != "" {
		req.Header.Set("X-EMLy-AppVersion", s.EMLyVersion)
	}
	if s.LoggedUser != "" {
		req.Header.Set("X-EMLy-LoggedUser", s.LoggedUser)
	}
	if s.LoggedUserState != "" {
		req.Header.Set("X-EMLy-LoggedUserState", s.LoggedUserState)
	}
	if !s.LoggedUserDisconnectedAt.IsZero() {
		req.Header.Set("X-EMLy-LoggedUserDisconnectedAt", s.LoggedUserDisconnectedAt.UTC().Format(time.RFC3339))
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
	// The watchdog runs from before the dial to the last body byte, so a
	// server that never answers, never sends headers or stops mid-body is
	// caught the same way - see SetupIdleTimeout.
	idle := s.setupIdleTimeout
	if idle <= 0 {
		idle = SetupIdleTimeout
	}
	stalled := fmt.Errorf("%w: no data received for %s", ErrSetupStalled, idle)
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	watchdog := time.AfterFunc(idle, func() { cancel(stalled) })
	defer watchdog.Stop()
	// A cancellation this function caused reports its cause, not a bare
	// "context canceled"; the caller's own cancellation is passed through.
	failed := func(msg string, err error) error {
		if cause := context.Cause(ctx); errors.Is(cause, ErrSetupStalled) {
			return cause
		}
		return fmt.Errorf("%s: %w", msg, err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, t.DownloadRef, nil)
	if err != nil {
		return fmt.Errorf("invalid download URL %q: %w", t.DownloadRef, err)
	}
	s.applyHeaders(req)

	resp, err := s.Client.Do(req)
	if err != nil {
		return failed("setup download failed", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusTooManyRequests {
		return retryLaterFrom(resp)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("setup download returned HTTP %d", resp.StatusCode)
	}

	dest, err := os.Create(destPath)
	if err != nil {
		return fmt.Errorf("failed to create %s: %w", destPath, err)
	}
	defer dest.Close()

	watchdog.Reset(idle) // the headers arrived: count from here
	var body io.Reader = &idleReader{r: resp.Body, timer: watchdog, idle: idle}
	if report := progressFrom(ctx); report != nil {
		report(0, resp.ContentLength)
		body = &progressReader{r: body, fn: report, total: resp.ContentLength}
	}
	n, err := io.Copy(dest, body)
	if err != nil {
		return failed("setup download interrupted", err)
	}
	// The transport already fails a body cut short of its Content-Length
	// (io.ErrUnexpectedEOF, above) - an admin aborting the download from the
	// dashboard to free its slot, or the server's own maximum download
	// duration running out. This states it outright rather than
	// leaving it to the transport; the SHA-256 check in download.Ensure is
	// the last line either way.
	if resp.ContentLength >= 0 && n != resp.ContentLength {
		return fmt.Errorf("setup download truncated: got %d of %d bytes", n, resp.ContentLength)
	}
	return dest.Close()
}

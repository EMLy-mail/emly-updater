package service

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"emlyupdater/internal/config"
	"emlyupdater/internal/policy"
)

const testManifest = `{"stableVersion":"1.2.3","stableDownload":"https://x/setup.exe","sha256Checksums":{"1.2.3":"abc"}}`

// manifestServer answers every request with status and, on 200, a valid
// EMLy manifest.
func manifestServer(t *testing.T, status int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		if status == http.StatusOK {
			_, _ = w.Write([]byte(testManifest))
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// deadURL is a base URL nothing listens on any more.
func deadURL(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()
	return url
}

// preferredUpdater builds a machine on the DC-RM2 site whose chain is
// "internal,external", pointed at the two given base URLs, with a single
// resolver attempt so a dead primary costs no backoff.
func preferredUpdater(t *testing.T, internalBase, externalBase string) *Updater {
	t.Helper()
	cfg := internalCfg(t, config.SourceInternal)
	cfg.InternalManifestURL = internalBase + "/v2/updates/manifest"
	cfg.ExternalManifestURL = externalBase + "/v2/updates/manifest"
	u := newTestUpdater(t, cfg, dcNamed("DC-RM2"), ipsOf("172.16.96.50"))
	u.clientWSWake = make(chan struct{}, 1)
	snap := u.Policy.Current()
	snap.Parsed.Global.Updater.Resolver = policy.ResolverSettings{Attempts: 1, BaseBackoffSeconds: 0}
	snap.Parsed.Global.ClientWS = policy.ClientWSSettings{Enabled: true, Commands: policy.DefaultClientWSCommands}
	return u
}

// A backup that answers while the head of the chain is unreachable leads the
// chain for the rest of the session: the next resolver starts from it, and
// the presence channel is pointed at it and woken up.
func TestReachableBackupBecomesPreferredForTheSession(t *testing.T) {
	ext := manifestServer(t, http.StatusOK)
	u := preferredUpdater(t, deadURL(t), ext.URL)

	cyc := u.beginCycle(context.Background(), true)
	if got := strings.Join(cyc.chain, ","); got != "internal,external" {
		t.Fatalf("chain = %q, want internal,external", got)
	}
	if got := u.clientWSTarget().server; got != "internal" {
		t.Fatalf("presence target before any poll = %q, want internal", got)
	}

	if _, _, _, err := u.resolveTarget(context.Background(), cyc, "stable"); err != nil {
		t.Fatalf("resolveTarget: %v", err)
	}
	if got := u.preferredServer(); got != "external" {
		t.Fatalf("preferred = %q, want external", got)
	}
	select {
	case <-u.clientWSWake:
	default:
		t.Error("presence supervisor was not woken")
	}

	// Next cycle: the working server is tried first.
	cyc = u.beginCycle(context.Background(), false)
	r := u.newResolver(cyc)
	if got := r.Primary.Name(); !strings.Contains(got, ext.URL) {
		t.Errorf("next primary = %q, want the external server", got)
	}
	if len(r.Fallbacks) != 1 || strings.Contains(r.Fallbacks[0].Name(), ext.URL) {
		t.Errorf("next fallbacks = %v, want the policy head only", r.Fallbacks)
	}
	target := u.clientWSTarget()
	if target.server != "external" || !strings.HasPrefix(target.url, "ws://"+strings.TrimPrefix(ext.URL, "http://")) {
		t.Errorf("presence target = %+v, want the external server", target)
	}
}

// When the head answers again the preference is dropped and the policy order
// is back.
func TestPreferenceIsDroppedWhenTheHeadAnswersAgain(t *testing.T) {
	head := manifestServer(t, http.StatusOK)
	ext := manifestServer(t, http.StatusOK)
	u := preferredUpdater(t, head.URL, ext.URL)
	name := "external"
	u.preferred.Store(&name)

	// The preferred server goes away; the head, now a fallback, serves.
	ext.Close()
	cyc := u.beginCycle(context.Background(), true)
	if _, _, _, err := u.resolveTarget(context.Background(), cyc, "stable"); err != nil {
		t.Fatalf("resolveTarget: %v", err)
	}
	if got := u.preferredServer(); got != "" {
		t.Errorf("preferred = %q, want none once the head answered", got)
	}
	if got := u.clientWSTarget().server; got != "internal" {
		t.Errorf("presence target = %q, want the policy head again", got)
	}
}

// A head that answers 404 is reachable - it just does not serve that
// document - so the fallback that serves it must not become preferred.
func TestNotFoundOnTheHeadDoesNotPinTheBackup(t *testing.T) {
	head := manifestServer(t, http.StatusNotFound)
	ext := manifestServer(t, http.StatusOK)
	u := preferredUpdater(t, head.URL, ext.URL)

	cyc := u.beginCycle(context.Background(), true)
	if _, _, _, err := u.resolveTarget(context.Background(), cyc, "stable"); err != nil {
		t.Fatalf("resolveTarget: %v", err)
	}
	if got := u.preferredServer(); got != "" {
		t.Errorf("preferred = %q, want none after a 404 on the head", got)
	}
}

// A preference the chain no longer lists (the machine moved site) is
// forgotten rather than applied.
func TestPreferenceOutsideTheChainIsForgotten(t *testing.T) {
	u := preferredUpdater(t, deadURL(t), deadURL(t))
	name := "some-other-site"
	u.preferred.Store(&name)

	cyc := u.beginCycle(context.Background(), true)
	if got := strings.Join(u.preferredChain(cyc), ","); got != "internal,external" {
		t.Errorf("chain = %q, want the policy order", got)
	}
	if got := u.preferredServer(); got != "" {
		t.Errorf("preferred = %q, want it forgotten", got)
	}
}

// A wake-up cuts a presence backoff short.
func TestClientWSIdleWakesOnPreferenceChange(t *testing.T) {
	u := preferredUpdater(t, deadURL(t), deadURL(t))
	u.wakeClientWS()
	start := time.Now()
	if !u.clientWSIdle(context.Background(), time.Hour) {
		t.Fatal("idle reported stop without a cancelled context")
	}
	if time.Since(start) > 5*time.Second {
		t.Error("idle was not woken")
	}
}

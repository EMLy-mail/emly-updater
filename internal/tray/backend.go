//go:build windows

package tray

import (
	"context"
	"os"
	"path/filepath"
	"sync"

	"emlyupdater/internal/config"
	"emlyupdater/internal/logging"
	"emlyupdater/internal/service"
)

// backend is the read-only half of the tray: config.ini as written, and the
// effective policy and product state the service itself would compute. It
// builds its own service.Updater - the same thing `emly-updater products`
// does - and never runs a cycle with it: nothing here downloads, installs,
// or writes under ProgramData. All calls are serialised by mu and run off
// the UI thread (machineinfo.Collect spawns PowerShell, Prepare can wait out
// the DC lookup's boot retry window, a manifest check goes to the network).
type backend struct {
	mu  sync.Mutex
	log *logging.Logger
	u   *service.Updater
}

func newBackend() *backend {
	return &backend{
		// Like `products`: a log of its own in the user's temp directory, never
		// the service's (ProgramData\...\logs is not writable by users anyway).
		log: logging.New(filepath.Join(os.TempDir(), "aryxd-agent-tray"), "", false),
	}
}

// snapshot is what the settings window shows.
type snapshot struct {
	cfg    *config.Config
	policy service.PolicyView
	err    error
}

// load re-reads config.ini and re-evaluates the policy. A new Updater every
// time: the file may have just been edited, and the policy cache rewritten
// by the service.
func (b *backend) load(ctx context.Context) snapshot {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.loadLocked(ctx)
}

func (b *backend) loadLocked(ctx context.Context) snapshot {
	cfg, err := config.Load(config.ConfigPath())
	if err != nil {
		return snapshot{err: err}
	}
	u := service.New(cfg, b.log, false)
	u.Prepare(ctx, false)
	b.u = u
	return snapshot{cfg: cfg, policy: u.EffectivePolicy()}
}

// products lists every product, loading the policy first if needed.
func (b *backend) products(ctx context.Context) ([]service.ProductRow, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.u == nil {
		if snap := b.loadLocked(ctx); snap.err != nil {
			return nil, snap.err
		}
	}
	return b.u.ProductRows(), nil
}

// check is the dry-run manifest check for one product, or for the agent
// itself with slug == agentSlug.
func (b *backend) check(ctx context.Context, slug string) (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.u == nil {
		if snap := b.loadLocked(ctx); snap.err != nil {
			return "", snap.err
		}
	}
	if slug == agentSlug {
		return b.u.CheckUpdater(ctx)
	}
	return b.u.CheckProduct(ctx, slug)
}

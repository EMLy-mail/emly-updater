//go:build windows

package tray

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"time"

	"emlyupdater/internal/config"
	"emlyupdater/internal/logging"
	"emlyupdater/internal/machineinfo"
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
	// machine is collected once per tray process: hostname, HWID, AD domain
	// and firmware strings do not change while it runs, and collecting them
	// costs process spawns (~5 s on a cold machine). Concurrent callers wait
	// for the first collection. Warmed up by prewarm.
	machine func() machineinfo.Info
}

// prewarmDelay holds the background machine-info collection back after the
// tray starts: it starts at logon, and the logon has better uses for the CPU.
const prewarmDelay = 30 * time.Second

func newBackend() *backend {
	// Like `products`: a log of its own in the user's temp directory, never
	// the service's (ProgramData\...\logs is not writable by users anyway).
	// At debug level, for the step-by-step timings of load: it is written
	// only when the settings window or a product check loads.
	log := logging.New(filepath.Join(os.TempDir(), "aryxd-agent-tray"), "", false)
	log.SetLevel("debug")
	b := &backend{log: log}
	b.machine = sync.OnceValue(func() machineinfo.Info { return service.CollectMachine(log) })
	return b
}

// prewarm collects the machine facts in the background, prewarmDelay after
// the tray starts, so the first settings window does not wait on them.
// Opening it sooner simply collects them then (the first caller does).
func (b *backend) prewarm(ctx context.Context) {
	select {
	case <-time.After(prewarmDelay):
	case <-ctx.Done():
		return
	}
	b.machine()
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
	start := time.Now()
	b.log.Debug("tray load started")
	cfg, err := config.Load(config.ConfigPath())
	b.log.Debug("tray load: config.ini read", "took", time.Since(start).String())
	if err != nil {
		b.log.Warn("tray load: config.ini unreadable", "error", err.Error())
		return snapshot{err: err}
	}
	step := time.Now()
	u := service.NewWithMachine(cfg, b.log, false, b.machine())
	b.log.Debug("tray load: updater built (machine info)", "took", time.Since(step).String())
	step = time.Now()
	u.Prepare(ctx, false)
	b.log.Debug("tray load: policy prepared", "took", time.Since(step).String())
	b.u = u
	pv := u.EffectivePolicy()
	b.log.Debug("tray load finished", "total", time.Since(start).String(),
		"governed", pv.Governed, "site", pv.Site, "servers", len(pv.Servers))
	return snapshot{cfg: cfg, policy: pv}
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

package service

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"emlyupdater/internal/machineinfo"
	"emlyupdater/internal/policy"
	"emlyupdater/internal/state"
)

// Prepare backs the tray and `products`: it must never write the service's
// files - here, move an invalid cache aside - and without bootRetry it must
// believe a failed DC lookup at once instead of sitting out the boot window.
func TestPrepareIsReadOnlyAndSkipsBootRetry(t *testing.T) {
	cfg := internalCfg(t, "internal")
	cfg.RemoteConfigEnabled = true
	cfg.DCLookupRetryAttempts, cfg.DCLookupRetryDelay = 3, time.Hour // would block the test

	dir := t.TempDir()
	cache := filepath.Join(dir, "remote-config.json")
	if err := os.WriteFile(cache, []byte("{not json"), 0644); err != nil {
		t.Fatal(err)
	}

	lookups := 0
	u := &Updater{
		Cfg:       cfg,
		Log:       testLogger(t),
		Machine:   machineinfo.Info{Hostname: "RM095", HWID: "HW-1", ADDomain: "tregcc.local"},
		CachePath: cache,
		Store:     &state.Store{Path: filepath.Join(dir, "state.json")},
		dcFn: func(string) (*machineinfo.DomainControllerInfo, error) {
			lookups++
			return nil, errors.New("domain unreachable")
		},
		ipsFn: ipsOf("172.16.96.50"),
	}
	u.Prepare(context.Background(), false)

	if lookups != 1 {
		t.Fatalf("DC lookups = %d, want 1 (no boot retry)", lookups)
	}
	if _, err := os.Stat(cache); err != nil {
		t.Fatalf("invalid cache was moved: %v", err)
	}
	if _, err := os.Stat(cache + ".bad"); !os.IsNotExist(err) {
		t.Fatalf("quarantine copy written: %v", err)
	}
	v := u.EffectivePolicy()
	if v.Governed || v.Source != policy.SourceDefault.String() {
		t.Fatalf("policy = %+v, want the config.ini-derived default", v)
	}
	if v.Updater.PollIntervalMinutes != 15 {
		t.Fatalf("poll = %d, want config.ini's 15", v.Updater.PollIntervalMinutes)
	}
}

package service

import (
	"context"
	"strings"
	"testing"

	"emlyupdater/internal/config"
)

// Disabled in config.ini means no window object at all, and every call on it
// - as Cycle, apply, install and applySelfUpdate make them - is a no-op.
func TestProgressUIDisabledIsNilAndInert(t *testing.T) {
	u := &Updater{Cfg: &config.Config{ProgressWindowEnabled: false}}
	p := u.newProgressUI(false, "1.8.0")
	if p != nil {
		t.Fatalf("newProgressUI = %+v with [progressWindow] enabled = false, want nil", p)
	}
	ctx := context.Background()
	if p.watch(ctx) != ctx {
		t.Error("watch on a disabled window must not attach a progress callback")
	}
	p.download(1, 2)
	p.installing()
	p.close()
	p.detach()
	u.endProgress()
}

func TestProgressUITexts(t *testing.T) {
	it := &progressUI{product: "EMLy", version: "1.8.0", lang: "it"}
	h, d := it.downloadText(3*1024*1024+512*1024, 10*1024*1024)
	if h != "Download di EMLy 1.8.0 in corso" || !strings.HasPrefix(d, "3,5 MB di 10,0 MB") {
		t.Errorf("it download = %q / %q", h, d)
	}
	if _, d := it.downloadText(1024*1024, -1); !strings.HasPrefix(d, "1,0 MB scaricati") {
		t.Errorf("it download, unknown size = %q", d)
	}
	if h, _ := it.installText(); h != "Installazione di EMLy 1.8.0 in corso" {
		t.Errorf("it install = %q", h)
	}
	if it.title() != "EMLy - Aggiornamento" {
		t.Errorf("it title = %q", it.title())
	}

	en := &progressUI{product: "AryxD Agent", version: "1.7.4", lang: "en", self: true}
	h, d = en.downloadText(512*1024, 1024*1024)
	if h != "Downloading AryxD Agent 1.7.4" || !strings.HasPrefix(d, "0.5 MB of 1.0 MB") {
		t.Errorf("en download = %q / %q", h, d)
	}
	if _, d := en.installText(); !strings.Contains(d, "EMLy remains available") {
		t.Errorf("en self install detail = %q: the agent's own update does not touch EMLy", d)
	}
}

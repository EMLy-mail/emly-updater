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

// Italian only, whatever EMLy's LANGUAGE is.
func TestProgressUITexts(t *testing.T) {
	p := &progressUI{product: "EMLy", version: "1.8.0"}
	h, d := p.downloadText(3*1024*1024+512*1024, 10*1024*1024)
	if h != "Download di EMLy 1.8.0 in corso" || !strings.HasPrefix(d, "3,5 MB di 10,0 MB") {
		t.Errorf("download = %q / %q", h, d)
	}
	if _, d := p.downloadText(1024*1024, -1); !strings.HasPrefix(d, "1,0 MB scaricati") {
		t.Errorf("download, unknown size = %q", d)
	}
	if h, d := p.installText(); h != "Installazione di EMLy 1.8.0 in corso" || !strings.Contains(d, "non è disponibile") {
		t.Errorf("install = %q / %q", h, d)
	}
	if p.title() != "EMLy - Aggiornamento" {
		t.Errorf("title = %q", p.title())
	}

	self := &progressUI{product: "AryxD Agent", version: "1.7.4", self: true}
	if h, d := self.installText(); h != "Installazione di AryxD Agent 1.7.4 in corso" || !strings.Contains(d, "EMLy resta utilizzabile") {
		t.Errorf("self install = %q / %q: the agent's own update does not touch EMLy", h, d)
	}
}

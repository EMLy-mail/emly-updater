package progresswin

import (
	"io"
	"os"
	"testing"
	"time"
)

// TestRunLive shows the real window for about ten seconds, driven exactly as
// the service drives it: a simulated download, then the install phase, then
// Close. Skipped unless EMLY_PROGRESS_WINDOW_TEST is set; it needs a desktop.
//
//	$env:EMLY_PROGRESS_WINDOW_TEST=1; go test ./internal/progresswin/ -run Live -v
//
// EMLY_PROGRESS_WINDOW_ICON names the icon's executable (default
// C:\3gIT\EMLy\EMLy.exe; a missing one shows the fallback).
//
// A test binary carries no manifest, so the bar here is the classic control
// and the install phase shows a full bar instead of a moving marquee. For the
// real look, drive build\EMLyUpdater.exe show-progress through its stdin.
func TestRunLive(t *testing.T) {
	if os.Getenv("EMLY_PROGRESS_WINDOW_TEST") == "" {
		t.Skip("set EMLY_PROGRESS_WINDOW_TEST=1 to show the real window")
	}
	icon := os.Getenv("EMLY_PROGRESS_WINDOW_ICON")
	if icon == "" {
		icon = `C:\3gIT\EMLy\EMLy.exe`
	}

	r, w := io.Pipe()
	go func() {
		defer w.Close()
		send := func(m Message) { _, _ = w.Write(Encode(m)) }
		for pc := 0; pc <= 100; pc += 2 {
			send(Message{Heading: "Download di EMLy 1.8.0 in corso",
				Detail: "L'operazione potrebbe richiedere alcuni minuti.", Percent: pc})
			time.Sleep(100 * time.Millisecond)
		}
		send(Message{Heading: "Installazione di EMLy 1.8.0 in corso",
			Detail: "Attendere il completamento. EMLy non è disponibile fino al termine.", Percent: Indeterminate})
		time.Sleep(5 * time.Second)
		send(Message{Close: true})
	}()

	start := time.Now()
	if err := Run(Options{Title: "EMLy - Aggiornamento", IconPath: icon}, r); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if d := time.Since(start); d < 9*time.Second {
		t.Errorf("window closed after %s, before the Close message", d)
	}
}

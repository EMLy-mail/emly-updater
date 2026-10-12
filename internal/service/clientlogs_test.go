package service

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"emlyupdater/internal/wsclient"
)

func decodeLogContent(t *testing.T, p wsclient.LogFile) string {
	t.Helper()
	if p.Encoding != wsclient.LogEncodingGzipBase64 {
		t.Fatalf("encoding = %q", p.Encoding)
	}
	gz, err := base64.StdEncoding.DecodeString(p.Content)
	if err != nil {
		t.Fatal(err)
	}
	zr, err := gzip.NewReader(bytes.NewReader(gz))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func writeLog(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "updater.log")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestExecuteCommandLogSendLatestWholeFile(t *testing.T) {
	u := newClientTestUpdater(t)
	withPolicy(t, u, wsclient.CmdLogSendLatest)
	content := "time=a level=INFO msg=one\ntime=b level=WARN msg=two\n"
	path := writeLog(t, content)
	u.logFilePathFn = func() string { return path }

	s := &fakeSession{}
	msg, cmd := cmdMsg(wsclient.CmdLogSendLatest, ``)
	u.executeCommand(context.Background(), s, msg, cmd)
	if len(s.acks) != 1 || !s.acks[0].Accepted || len(s.results) != 1 || s.results[0].Status != wsclient.ResultOK {
		t.Fatalf("acks=%+v results=%+v", s.acks, s.results)
	}
	if s.results[0].Truncated {
		t.Fatal("a small file must not be truncated")
	}
	var p wsclient.LogFile
	if err := json.Unmarshal(s.results[0].Payload, &p); err != nil {
		t.Fatal(err)
	}
	if p.Name != "updater.log" || p.SizeBytes != int64(len(content)) || p.Offset != 0 || p.IncludedBytes != int64(len(content)) {
		t.Fatalf("payload = %+v", p)
	}
	if got := decodeLogContent(t, p); got != content {
		t.Fatalf("content = %q", got)
	}
}

// A log that does not fit loses its start, never its end, and is cut at a
// line boundary; the frame stays under the budget.
func TestSendLatestLogKeepsTheTail(t *testing.T) {
	u := newClientTestUpdater(t)
	// Random hex compresses about 2:1 - far worse than a real log - so a
	// modest file is already well past what one frame holds.
	rng := rand.New(rand.NewPCG(1, 2))
	var b strings.Builder
	for i := 0; b.Len() < 600_000; i++ {
		fmt.Fprintf(&b, "line=%07d data=%016x%016x\n", i, rng.Uint64(), rng.Uint64())
	}
	content := b.String()
	path := writeLog(t, content)
	u.logFilePathFn = func() string { return path }

	payload, e, truncated := u.sendLatestLog()
	if e != nil || !truncated {
		t.Fatalf("e=%v truncated=%v", e, truncated)
	}
	raw, _ := json.Marshal(payload)
	if len(raw) > resultPayloadBudget {
		t.Fatalf("payload is %d bytes, budget %d", len(raw), resultPayloadBudget)
	}
	p := payload.(wsclient.LogFile)
	got := decodeLogContent(t, p)
	if !strings.HasSuffix(content, got) || !strings.HasPrefix(got, "line=") || content[p.Offset-1] != '\n' {
		t.Fatalf("not a line-aligned tail: offset=%d starts %q", p.Offset, got[:min(len(got), 40)])
	}
	if p.Offset+p.IncludedBytes != p.SizeBytes || int64(len(got)) != p.IncludedBytes {
		t.Fatalf("offset=%d included=%d size=%d decoded=%d", p.Offset, p.IncludedBytes, p.SizeBytes, len(got))
	}
	// The search should not give away much: at least 3/4 of what fits.
	if len(raw) < resultPayloadBudget*3/4 {
		t.Fatalf("payload only %d bytes of a %d budget", len(raw), resultPayloadBudget)
	}
}

func TestSendLatestLogMissingFile(t *testing.T) {
	u := newClientTestUpdater(t)
	u.logFilePathFn = func() string { return filepath.Join(t.TempDir(), "updater.log") }
	if _, e, _ := u.sendLatestLog(); e == nil || e.Code != wsclient.ErrNotFound {
		t.Fatalf("e = %+v", e)
	}
}

func TestFitLogTailSkipsThePartialFirstLine(t *testing.T) {
	data := []byte("tial\nwhole one\nwhole two\n")
	all := func(string, int) bool { return true }
	if got := fitLogTail(data, true, all); got != 5 {
		t.Fatalf("midLine start = %d, want 5", got)
	}
	if got := fitLogTail(data, false, all); got != 0 {
		t.Fatalf("start = %d, want 0", got)
	}
	if got := fitLogTail(data, false, func(string, int) bool { return false }); got != len(data) {
		t.Fatalf("nothing fits -> %d, want %d", got, len(data))
	}
}

// log.send_latest is opt-in: the default allowlist does not carry it.
func TestLogSendLatestNotAllowedByDefault(t *testing.T) {
	u := newClientTestUpdater(t)
	s := &fakeSession{}
	msg, cmd := cmdMsg(wsclient.CmdLogSendLatest, ``)
	u.executeCommand(context.Background(), s, msg, cmd)
	if len(s.acks) != 1 || s.acks[0].Accepted || s.acks[0].Error.Code != wsclient.ErrDisabledByPolicy {
		t.Fatalf("acks = %+v", s.acks)
	}
}

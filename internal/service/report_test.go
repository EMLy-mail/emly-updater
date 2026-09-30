package service

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestReportProductsListsDetectionAndManifest(t *testing.T) {
	srv := newProductServer(t)
	h := newProductHarness(t, srv)
	var out bytes.Buffer
	if err := h.u.ReportProducts(context.Background(), &out, true); err != nil {
		t.Fatalf("ReportProducts: %v", err)
	}
	text := out.String()
	for _, want := range []string{"3g-rocketchat", "installed 1.0.0 (file:version.txt)", "1.1.0", "emly", "2.0.0"} {
		if !strings.Contains(text, want) {
			t.Errorf("report lacks %q:\n%s", want, text)
		}
	}
	if len(h.driver.installs) != 0 {
		t.Fatalf("the report installed something: %v", h.driver.installs)
	}
}

package authenticode

import (
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Environment variables gating the one test here that exercises the real
// wintrust/crypt32 path. Everything else in this package is pure Go and runs
// anywhere; this needs an actual Authenticode-signed file on the machine,
// which CI has no reliable way to provide.
const (
	liveFileEnv       = "EMLY_AUTHENTICODE_TEST_FILE"
	liveThumbprintEnv = "EMLY_AUTHENTICODE_TEST_THUMBPRINT"
)

// TestVerifyAgainstLiveSignature exercises what no pure test can reach:
// CryptQueryObject reading a real embedded PKCS#7 signature, CryptMsgGetParam
// identifying the actual signer, and WinVerifyTrust validating the digest and
// the chain.
//
// Skipped unless EMLY_AUTHENTICODE_TEST_FILE names a signed file. Run it after
// any change to verify.go, and as the check that a freshly built installer is
// signed the way the self-update path expects:
//
//	$env:EMLY_AUTHENTICODE_TEST_FILE = "installer\Output\EMLyUpdater_Installer_1.5.0.exe"
//	go test ./internal/authenticode/ -run Live -v
//
// With EMLY_AUTHENTICODE_TEST_THUMBPRINT also set (hex, as the Windows
// certificate UI shows it) the signer is asserted to be that certificate;
// without it the discovered thumbprint is only reported, and the test still
// proves the signature verifies and that a wrong pin is refused.
//
// Note this covers embedded signatures only, which is what signtool produces
// for our own binaries. A catalog-signed file (most Windows system binaries)
// has no embedded PKCS#7 and is correctly rejected as unsigned.
func TestVerifyAgainstLiveSignature(t *testing.T) {
	path := os.Getenv(liveFileEnv)
	if path == "" {
		t.Skipf("set %s to a signed file to run this test", liveFileEnv)
	}

	thumbprint, err := signerThumbprint(path)
	if err != nil {
		t.Fatalf("could not read the signer of %s: %v", path, err)
	}
	got := hex.EncodeToString(thumbprint)
	t.Logf("%s is signed by certificate %s", path, got)

	if want := os.Getenv(liveThumbprintEnv); want != "" {
		if !strings.EqualFold(got, want) {
			t.Fatalf("signer thumbprint = %s, want %s", got, want)
		}
	}

	// Pinned to its actual signer, the file must verify.
	if err := Verify(path, thumbprint); err != nil {
		t.Fatalf("Verify with the correct pin failed: %v", err)
	}

	// Pinned to anything else, it must not - a valid signature by the wrong
	// publisher is exactly the case the pin exists to catch.
	wrong := Thumbprint([]byte("some other certificate"))
	if err := Verify(path, wrong); !errors.Is(err, ErrUntrusted) {
		t.Fatalf("Verify with a wrong pin returned %v, want ErrUntrusted", err)
	}

	// And the case the whole package exists for: the right publisher's
	// certificate still attached to bytes that were changed afterwards.
	tampered := tamperedCopy(t, path)
	err = Verify(tampered, thumbprint)
	if !errors.Is(err, ErrUntrusted) {
		t.Fatalf("Verify accepted a modified file: %v", err)
	}
	if !strings.Contains(err.Error(), "does not match its signature") {
		t.Errorf("tampering reported as %q, want it to name the digest mismatch", err)
	}
}

// tamperedCopy copies path into the test's temp dir and flips a byte in the
// middle of it, leaving the signature intact but no longer covering the file.
func tamperedCopy(t *testing.T, path string) string {
	t.Helper()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	data[len(data)/2] ^= 0xFF

	dst := filepath.Join(t.TempDir(), "tampered.exe")
	if err := os.WriteFile(dst, data, 0644); err != nil {
		t.Fatal(err)
	}
	return dst
}

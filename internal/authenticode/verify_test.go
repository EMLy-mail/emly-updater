package authenticode

import (
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// The thumbprint of the embedded 3gIT code-signing certificate, as Windows
// displays it. Recomputing it from the certificate is what the service does;
// this pins the algorithm and encoding so a change to Thumbprint cannot
// silently start producing something the pin no longer matches.
const threeGITThumbprint = "9a1658d5e5ca7f6216b86bac23e0cd048e8f215e"

func TestThumbprintMatchesWindows(t *testing.T) {
	der, err := os.ReadFile(filepath.Join("..", "cert", "3GITInnovation.cer"))
	if err != nil {
		t.Fatal(err)
	}
	if got := hex.EncodeToString(Thumbprint(der)); got != threeGITThumbprint {
		t.Fatalf("Thumbprint = %s, want %s (as shown by certutil / the Windows certificate UI)",
			got, threeGITThumbprint)
	}
}

// Verifying against no pin at all would accept any signature: it must be
// refused rather than treated as "nothing to check".
func TestVerifyRefusesEmptyPin(t *testing.T) {
	err := Verify(filepath.Join(t.TempDir(), "whatever.exe"), nil)
	if !errors.Is(err, ErrUntrusted) {
		t.Fatalf("error = %v, want ErrUntrusted", err)
	}
}

// An unsigned file must be rejected, and the message must say so plainly -
// this is what an operator reads when a self-update refuses to run.
func TestVerifyRejectsUnsignedFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "unsigned.exe")
	if err := os.WriteFile(path, []byte("MZ not really a PE"), 0644); err != nil {
		t.Fatal(err)
	}

	err := Verify(path, Thumbprint([]byte("any certificate")))
	if !errors.Is(err, ErrUntrusted) {
		t.Fatalf("error = %v, want ErrUntrusted", err)
	}
}

func TestVerifyRejectsMissingFile(t *testing.T) {
	err := Verify(filepath.Join(t.TempDir(), "gone.exe"), Thumbprint([]byte("x")))
	if !errors.Is(err, ErrUntrusted) {
		t.Fatalf("error = %v, want ErrUntrusted", err)
	}
}

func TestIsHRESULT(t *testing.T) {
	err := error(syscall.Errno(certUntrustedRoot))
	if !isHRESULT(err, certUntrustedRoot) {
		t.Error("isHRESULT did not recognise CERT_E_UNTRUSTEDROOT")
	}
	if isHRESULT(err, trustBadDigest) {
		t.Error("isHRESULT matched the wrong HRESULT")
	}
	if isHRESULT(errors.New("not an errno"), certUntrustedRoot) {
		t.Error("isHRESULT matched a non-syscall error")
	}
}

// The two failures an operator is most likely to hit must not surface as a
// bare hex HRESULT.
func TestDescribeTrustError(t *testing.T) {
	cases := map[uint32]string{
		trustBadDigest:    "does not match its signature",
		trustNoSignature:  "not signed",
		certUntrustedRoot: "does not chain to the expected certificate",
	}
	for hresult, want := range cases {
		got := describeTrustError(syscall.Errno(hresult))
		if !strings.Contains(got, want) {
			t.Errorf("describeTrustError(%#x) = %q, want it to mention %q", hresult, got, want)
		}
	}

	other := errors.New("some other failure")
	if got := describeTrustError(other); got != other.Error() {
		t.Errorf("describeTrustError passed through as %q", got)
	}
}

func TestSubtleEqual(t *testing.T) {
	a := Thumbprint([]byte("same"))
	if !subtleEqual(a, Thumbprint([]byte("same"))) {
		t.Error("equal thumbprints did not compare equal")
	}
	if subtleEqual(a, Thumbprint([]byte("different"))) {
		t.Error("different thumbprints compared equal")
	}
}

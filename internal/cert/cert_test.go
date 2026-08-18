package cert

import (
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"testing"
	"time"
)

// wantThumbprint is the SHA-256 of the DER encoding this package is expected
// to embed. Swapping the .cer without updating this constant - and the design
// doc that records it - fails the build on purpose.
const wantThumbprint = "bfd4d8090131e81e11e3ec216839eb709389d97acc2a5b53c46fa710be7268eb"

// expiryMargin is how long before NotAfter these tests start failing. The
// certificate rotates roughly annually, so failing two months early turns a
// silent expiry - which would quietly restore the "Unknown publisher" prompt
// on every machine - into a red build with time left to act.
const expiryMargin = 60 * 24 * time.Hour

func TestEmbeddedParses(t *testing.T) {
	c, der, err := Embedded()
	if err != nil {
		t.Fatalf("Embedded() failed: %v", err)
	}
	if c == nil {
		t.Fatal("Embedded() returned a nil certificate")
	}
	if len(der) == 0 {
		t.Fatal("Embedded() returned empty DER")
	}
}

func TestEmbeddedThumbprint(t *testing.T) {
	_, der, err := Embedded()
	if err != nil {
		t.Fatalf("Embedded() failed: %v", err)
	}
	sum := sha256.Sum256(der)
	if got := hex.EncodeToString(sum[:]); got != wantThumbprint {
		t.Errorf("embedded certificate thumbprint = %s, want %s\n"+
			"If the certificate was deliberately rotated, update wantThumbprint "+
			"and section 4 of the design doc.", got, wantThumbprint)
	}
}

func TestEmbeddedIsCodeSigning(t *testing.T) {
	c, _, err := Embedded()
	if err != nil {
		t.Fatalf("Embedded() failed: %v", err)
	}
	for _, eku := range c.ExtKeyUsage {
		if eku == x509.ExtKeyUsageCodeSigning {
			return
		}
	}
	t.Errorf("embedded certificate has no Code Signing EKU, got %v", c.ExtKeyUsage)
}

func TestEmbeddedIsSelfSigned(t *testing.T) {
	c, _, err := Embedded()
	if err != nil {
		t.Fatalf("Embedded() failed: %v", err)
	}
	if c.Subject.String() != c.Issuer.String() {
		t.Errorf("certificate is not self-signed: subject %q, issuer %q",
			c.Subject, c.Issuer)
	}
}

func TestEmbeddedIsAlreadyValid(t *testing.T) {
	c, _, err := Embedded()
	if err != nil {
		t.Fatalf("Embedded() failed: %v", err)
	}
	if now := time.Now(); now.Before(c.NotBefore) {
		t.Errorf("certificate is not valid yet: NotBefore = %s, now = %s",
			c.NotBefore.Format(time.RFC3339), now.Format(time.RFC3339))
	}
}

func TestEmbeddedNotNearingExpiry(t *testing.T) {
	c, _, err := Embedded()
	if err != nil {
		t.Fatalf("Embedded() failed: %v", err)
	}
	deadline := c.NotAfter.Add(-expiryMargin)
	if time.Now().After(deadline) {
		t.Errorf("embedded code-signing certificate expires %s, which is less "+
			"than %d days away.\nRotate it: issue a new certificate, replace "+
			"certs/3GITInnovation.cer and internal/cert/3GITInnovation.cer, "+
			"update wantThumbprint, and cut a release.",
			c.NotAfter.Format("2006-01-02"), int(expiryMargin.Hours()/24))
	}
}

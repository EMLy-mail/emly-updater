package cert

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"os"
	"runtime"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// liveEnv gates the one test in this package that touches the real Windows
// certificate stores. Everything else here is pure Go and runs anywhere;
// this needs administrator rights, which CI does not have.
const liveEnv = "EMLY_CERT_STORE_TEST"

// TestEnsureAgainstLiveStores exercises the crypt32 path that no other test
// can reach: Ensure opening and writing the real Root and TrustedPublisher
// stores, for the machine and - addressed by SID, the way the service reaches
// the console user - for the current user.
//
// Skipped unless EMLY_CERT_STORE_TEST is set, because it needs an elevated
// shell. To run it:
//
//	# from an elevated shell, in the repo root
//	EMLY_CERT_STORE_TEST=1 go test ./internal/cert/ -run Live -v
//
// It never installs the real 3gIT certificate: it generates a throwaway
// self-signed certificate, asserts it lands in all four stores, asserts a
// second Ensure is a no-op, then removes it again and confirms the stores are
// back as they were. Run it after any change to store.go or target.go, and as
// the quick check when rotating the certificate.
func TestEnsureAgainstLiveStores(t *testing.T) {
	if os.Getenv(liveEnv) == "" {
		t.Skipf("set %s=1 and run from an elevated shell to exercise the real Windows stores", liveEnv)
	}

	sid, err := currentUserSID()
	if err != nil {
		t.Fatalf("cannot resolve the current user's SID: %v", err)
	}
	t.Logf("current user SID: %s", sid)

	der := throwawayCert(t)
	targets := append(MachineTargets(), UserTargets(sid)...)

	// Make sure a previous interrupted run cannot make this one pass or fail
	// for the wrong reason.
	removeFromAll(t, der, targets)
	t.Cleanup(func() { removeFromAll(t, der, targets) })

	// First Ensure: every store should be written. Deliberately not fatal on
	// error - Ensure keeps going past a failed target, and which stores landed
	// is the whole diagnostic value here. Report them all, then decide.
	installed, err := Ensure(der, targets, func(format string, args ...any) {
		t.Logf(format, args...)
	})

	missing := 0
	for _, tg := range targets {
		if inStore(t, der, tg) {
			t.Logf("  PRESENT  %s", tg)
			continue
		}
		missing++
		t.Errorf("  MISSING  %s", tg)
	}
	if err != nil {
		t.Errorf("Ensure reported: %v", err)
	}
	if missing > 0 {
		t.Fatalf("%d of %d stores were not written. Access-denied on a LocalMachine "+
			"store means the shell is not elevated; a User store failing while the "+
			"LocalMachine ones succeed would mean CERT_SYSTEM_STORE_UNPROTECTED_FLAG "+
			"is not doing its job.", missing, len(targets))
	}
	if len(installed) != len(targets) {
		t.Fatalf("every store holds the certificate but Ensure only reported %d of %d: %v",
			len(installed), len(targets), installed)
	}

	// Second Ensure: CERT_STORE_ADD_NEW must report CRYPT_E_EXISTS everywhere,
	// which Ensure turns into "wrote nothing". This is the property the poll
	// loop relies on to stay quiet across 96 cycles a day.
	again, err := Ensure(der, targets, func(format string, args ...any) {
		t.Errorf("second Ensure wrote a store it should have skipped: "+format, args...)
	})
	if err != nil {
		t.Fatalf("second Ensure failed: %v", err)
	}
	if len(again) != 0 {
		t.Errorf("second Ensure wrote %d stores, want 0 (not idempotent): %v", len(again), again)
	}
}

// currentUserSID returns the SID of the process's own token. The service uses
// notify.ConsoleUserSID instead, but both feed UserTargets the same way, so
// this exercises the same CERT_SYSTEM_STORE_USERS path without needing
// SeTcbPrivilege.
func currentUserSID() (string, error) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return "", err
	}
	return user.User.Sid.String(), nil
}

// throwawayCert builds a short-lived self-signed certificate that exists only
// for this test, so a run never installs the real 3gIT certificate as a side
// effect. ECDSA because it generates instantly.
func throwawayCert(t *testing.T) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("key generation failed: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: "EMLyUpdater store test - safe to delete"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("certificate generation failed: %v", err)
	}
	return der
}

// openTarget opens tg the same way addToStore does, so the test observes the
// stores through the flags production actually uses.
func openTarget(t *testing.T, tg Target) (windows.Handle, error) {
	t.Helper()
	name, err := windows.UTF16PtrFromString(tg.StoreName())
	if err != nil {
		return 0, err
	}
	h, err := windows.CertOpenStore(
		windows.CERT_STORE_PROV_SYSTEM, 0, 0, tg.openFlags(), uintptr(unsafe.Pointer(name)))
	runtime.KeepAlive(name)
	return h, err
}

// encoded copies a context's DER out into a Go slice.
func encoded(ctx *windows.CertContext) []byte {
	return append([]byte(nil), unsafe.Slice(ctx.EncodedCert, ctx.Length)...)
}

// inStore reports whether der is present in tg.
func inStore(t *testing.T, der []byte, tg Target) bool {
	t.Helper()
	store, err := openTarget(t, tg)
	if err != nil {
		t.Errorf("cannot open %s: %v", tg, err)
		return false
	}
	defer windows.CertCloseStore(store, 0)

	var prev *windows.CertContext
	for {
		ctx, err := windows.CertEnumCertificatesInStore(store, prev)
		if err != nil || ctx == nil {
			return false
		}
		if string(encoded(ctx)) == string(der) {
			windows.CertFreeCertificateContext(ctx)
			return true
		}
		prev = ctx
	}
}

// removeFromAll deletes der from every target, ignoring stores where it is
// absent. Used both to clean up after the test and to normalise the starting
// state before it.
func removeFromAll(t *testing.T, der []byte, targets []Target) {
	t.Helper()
	for _, tg := range targets {
		store, err := openTarget(t, tg)
		if err != nil {
			continue // reported by the test proper, not here
		}
		var prev *windows.CertContext
		for {
			ctx, err := windows.CertEnumCertificatesInStore(store, prev)
			if err != nil || ctx == nil {
				break
			}
			if string(encoded(ctx)) != string(der) {
				prev = ctx
				continue
			}
			// CertDeleteCertificateFromStore frees the context it is given and
			// invalidates the enumeration, so hand it a duplicate and start over.
			dup := windows.CertDuplicateCertificateContext(ctx)
			windows.CertFreeCertificateContext(ctx)
			if err := windows.CertDeleteCertificateFromStore(dup); err != nil {
				t.Logf("could not delete the test certificate from %s: %v", tg, err)
			}
			prev = nil
		}
		windows.CertCloseStore(store, 0)
	}
}

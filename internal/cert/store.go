package cert

import (
	"errors"
	"fmt"
	"runtime"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// cryptEExists is CRYPT_E_EXISTS. Under CERT_STORE_ADD_NEW the store returns
// it when it already holds this exact certificate, and succeeds when it adds
// it - so the return value alone distinguishes "already there" from "just
// installed", atomically. A preceding CertFindCertificateInStore would add a
// lookup and a race window between check and add for no benefit.
//
// Declared here as a plain constant rather than used via
// windows.CRYPT_E_EXISTS, which is typed windows.Handle and does not compare
// against the syscall.Errno the store APIs actually return.
const cryptEExists = 0x80092005

// Ensure adds der to every target store that does not already hold it.
//
// It is idempotent: re-running it once the certificate is installed writes
// nothing and returns an empty slice. logf is called once per store actually
// written; callers that poll should log it below Info.
//
// A failure on one target does not skip the others - installing into the
// machine stores is still worth doing when the user's hive is unreachable, and
// vice versa. The first error is returned once every target has been tried.
func Ensure(der []byte, targets []Target, logf func(string, ...any)) ([]string, error) {
	if len(der) == 0 {
		return nil, errors.New("refusing to install an empty certificate")
	}

	ctx, err := windows.CertCreateCertificateContext(
		windows.X509_ASN_ENCODING|windows.PKCS_7_ASN_ENCODING,
		&der[0], uint32(len(der)))
	if err != nil {
		return nil, fmt.Errorf("certificate rejected by CertCreateCertificateContext: %w", err)
	}
	defer windows.CertFreeCertificateContext(ctx)

	var (
		installed []string
		firstErr  error
	)
	for _, t := range targets {
		added, err := addToStore(ctx, t)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if added {
			installed = append(installed, t.String())
			logf("installed code-signing certificate into %s", t.String())
		}
	}
	return installed, firstErr
}

// addToStore opens t for writing and adds ctx under CERT_STORE_ADD_NEW.
// Returns false with a nil error when the certificate is already present.
func addToStore(ctx *windows.CertContext, t Target) (bool, error) {
	name, err := windows.UTF16PtrFromString(t.StoreName())
	if err != nil {
		return false, fmt.Errorf("invalid store name %q: %w", t.StoreName(), err)
	}

	// CERT_STORE_READONLY_FLAG is deliberately absent: this store is written.
	store, err := windows.CertOpenStore(
		windows.CERT_STORE_PROV_SYSTEM,
		0, // encoding type: unused for system stores
		0, // hCryptProv: none
		t.openFlags(),
		uintptr(unsafe.Pointer(name)),
	)
	// name is passed as a uintptr, which the garbage collector does not treat
	// as a live reference - keep it alive across the call explicitly.
	runtime.KeepAlive(name)
	if err != nil {
		return false, fmt.Errorf("cannot open certificate store %s: %w", t, err)
	}
	defer windows.CertCloseStore(store, 0)

	// CERT_STORE_ADD_NEW compares the whole certificate, not the subject, so
	// during a rotation the old and new 3gIT certificates coexist. That is
	// intended: signatures made with the old one keep validating.
	err = windows.CertAddCertificateContextToStore(store, ctx, windows.CERT_STORE_ADD_NEW, nil)
	if err == nil {
		return true, nil
	}

	var errno syscall.Errno
	if errors.As(err, &errno) && uint32(errno) == cryptEExists {
		return false, nil
	}
	return false, fmt.Errorf("cannot add certificate to %s: %w", t, err)
}

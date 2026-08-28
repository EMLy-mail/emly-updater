// Package authenticode verifies that a file carries a valid Authenticode
// signature made by one specific certificate, identified by its SHA-1
// thumbprint.
//
// It exists for the self-update path. The updater downloads its own installer
// and runs it as LocalSystem, and that installer replaces the updater's own
// executable - the one thing on the machine that must not be attacker
// controlled. A SHA256 from the manifest is not enough on its own: the
// internal manifest source is plain HTTP by default, so whoever can serve a
// tampered setup can serve a matching checksum with it. A signature they
// cannot produce closes that gap.
//
// Two independent checks have to pass:
//
//   - WinVerifyTrust, which proves the signature actually covers these bytes
//     (and that the chain builds);
//   - the signer's thumbprint equals the pinned one, which proves *who* signed.
//
// The pin is what carries the trust here, not the Windows trust stores.
// WinVerifyTrust accepts anything chaining to any trusted root, which on a
// domain PC means every public CA and every certificate an administrator (or
// an attacker with administrator rights) has added; the 3gIT certificate is
// self-signed, so trusting the store alone would be both too permissive and,
// on a machine where certificate.enabled is false, too strict.
package authenticode

import (
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// HRESULTs WinVerifyTrust returns that this package names explicitly.
const (
	// certUntrustedRoot is CERT_E_UNTRUSTEDROOT: the chain ends in a root the
	// machine does not trust. Expected whenever the self-signed 3gIT
	// certificate is not in the Root store - which is the normal state on a
	// machine with certificate.enabled = false.
	certUntrustedRoot = 0x800B0109
	// trustBadDigest is TRUST_E_BAD_DIGEST: the file does not match what was
	// signed. Tampering, always fatal.
	trustBadDigest = 0x80096010
	// trustNoSignature is TRUST_E_NOSIGNATURE: the file carries no signature
	// at all.
	trustNoSignature = 0x800B0100
)

// cmsgSignerCertInfoParam is CMSG_SIGNER_CERT_INFO_PARAM: the issuer and
// serial number identifying the certificate that actually produced the
// signature, as opposed to the other certificates the message carries.
const cmsgSignerCertInfoParam = 7

var (
	crypt32              = windows.NewLazySystemDLL("crypt32.dll")
	procCryptMsgGetParam = crypt32.NewProc("CryptMsgGetParam")
	procCryptMsgClose    = crypt32.NewProc("CryptMsgClose")
)

// ErrUntrusted reports that a file's signature is missing, invalid, or was not
// made by the pinned certificate. Every rejection wraps it, so callers can
// tell "this binary must not be executed" from an I/O problem.
var ErrUntrusted = errors.New("file is not signed by the expected certificate")

// Verify reports whether path carries a valid Authenticode signature made by
// the certificate whose SHA-1 thumbprint is pinned.
//
// CERT_E_UNTRUSTEDROOT is the one WinVerifyTrust failure tolerated, and only
// when the thumbprint matches: reaching that error means the signature itself
// verified against the file and the chain was built all the way to the pinned
// certificate, which is precisely the trust decision being made here. Every
// other failure - a broken digest, no signature, an explicitly distrusted
// publisher - is fatal.
func Verify(path string, pinned []byte) error {
	if len(pinned) == 0 {
		return fmt.Errorf("%w: no certificate pinned to verify against", ErrUntrusted)
	}

	thumbprint, err := signerThumbprint(path)
	if err != nil {
		return err
	}
	pinMatches := len(thumbprint) == len(pinned) && subtleEqual(thumbprint, pinned)

	trustErr := verifyTrust(path)
	if trustErr != nil && !(isHRESULT(trustErr, certUntrustedRoot) && pinMatches) {
		return fmt.Errorf("%w: %s", ErrUntrusted, describeTrustError(trustErr))
	}
	if !pinMatches {
		return fmt.Errorf("%w: signed by %s, expected %s",
			ErrUntrusted, hex.EncodeToString(thumbprint), hex.EncodeToString(pinned))
	}
	return nil
}

// Thumbprint returns the SHA-1 thumbprint of a DER-encoded certificate, in the
// same form Verify pins against and the Windows certificate UI displays.
func Thumbprint(der []byte) []byte {
	sum := sha1.Sum(der)
	return sum[:]
}

// verifyTrust runs the standard Authenticode policy over path.
//
// Revocation checking is deliberately off: a site whose machines reach only
// their internal mirror has no route to a CRL or OCSP responder, and the
// check would stall the call for its full network timeout on every cycle
// before failing open anyway. Revocation is not what this package relies on -
// the pin is.
//
// WTD_STATEACTION_VERIFY allocates state that a second call with
// WTD_STATEACTION_CLOSE must release, or the process leaks a handle per
// verification.
func verifyTrust(path string) error {
	filePath, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	fileInfo := &windows.WinTrustFileInfo{
		Size:     uint32(unsafe.Sizeof(windows.WinTrustFileInfo{})),
		FilePath: filePath,
	}
	data := &windows.WinTrustData{
		Size:                            uint32(unsafe.Sizeof(windows.WinTrustData{})),
		UIChoice:                        windows.WTD_UI_NONE,
		RevocationChecks:                windows.WTD_REVOKE_NONE,
		UnionChoice:                     windows.WTD_CHOICE_FILE,
		StateAction:                     windows.WTD_STATEACTION_VERIFY,
		FileOrCatalogOrBlobOrSgnrOrCert: unsafe.Pointer(fileInfo),
	}

	verifyErr := windows.WinVerifyTrustEx(windows.InvalidHWND, &windows.WINTRUST_ACTION_GENERIC_VERIFY_V2, data)

	data.StateAction = windows.WTD_STATEACTION_CLOSE
	_ = windows.WinVerifyTrustEx(windows.InvalidHWND, &windows.WINTRUST_ACTION_GENERIC_VERIFY_V2, data)

	return verifyErr
}

// signerThumbprint returns the SHA-1 thumbprint of the certificate that
// produced path's embedded Authenticode signature.
//
// It asks the signed message which certificate signed it (issuer + serial)
// and then looks that one up among the certificates the message carries.
// Enumerating the message's certificates and looking for the expected one
// would not do: a signature can carry any certificate alongside the real
// signer's, so finding ours in the bag proves nothing about who signed.
func signerThumbprint(path string) ([]byte, error) {
	objectName, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}

	var store, msg windows.Handle
	err = windows.CryptQueryObject(
		windows.CERT_QUERY_OBJECT_FILE,
		unsafe.Pointer(objectName),
		windows.CERT_QUERY_CONTENT_FLAG_PKCS7_SIGNED_EMBED,
		windows.CERT_QUERY_FORMAT_FLAG_BINARY,
		0,
		nil, nil, nil,
		&store, &msg, nil,
	)
	if err != nil {
		return nil, fmt.Errorf("%w: no readable Authenticode signature on %s: %v", ErrUntrusted, path, err)
	}
	defer windows.CertCloseStore(store, 0)
	defer cryptMsgClose(msg)

	certInfo, err := signerCertInfo(msg)
	if err != nil {
		return nil, err
	}

	ctx, err := windows.CertFindCertificateInStore(
		store,
		windows.X509_ASN_ENCODING|windows.PKCS_7_ASN_ENCODING,
		0,
		windows.CERT_FIND_SUBJECT_CERT,
		unsafe.Pointer(certInfo),
		nil,
	)
	if err != nil {
		return nil, fmt.Errorf("%w: the signer's certificate is not in the signature of %s: %v", ErrUntrusted, path, err)
	}
	defer windows.CertFreeCertificateContext(ctx)

	der := unsafe.Slice(ctx.EncodedCert, ctx.Length)
	return Thumbprint(der), nil
}

// signerCertInfo reads CMSG_SIGNER_CERT_INFO_PARAM out of a signed message: a
// CERT_INFO whose issuer and serial number identify the signing certificate.
//
// The first call sizes the buffer, the second fills it - the usual two-step
// crypt32 convention. The returned pointer aliases the byte slice, which is
// kept alive by the caller holding the value it points into.
func signerCertInfo(msg windows.Handle) (*windows.CertInfo, error) {
	var size uint32
	if err := cryptMsgGetParam(msg, cmsgSignerCertInfoParam, 0, nil, &size); err != nil {
		return nil, fmt.Errorf("%w: cannot read the signer from the signature: %v", ErrUntrusted, err)
	}
	if size < uint32(unsafe.Sizeof(windows.CertInfo{})) {
		return nil, fmt.Errorf("%w: signer information is %d bytes, too short to be a CERT_INFO", ErrUntrusted, size)
	}

	buf := make([]byte, size)
	if err := cryptMsgGetParam(msg, cmsgSignerCertInfoParam, 0, &buf[0], &size); err != nil {
		return nil, fmt.Errorf("%w: cannot read the signer from the signature: %v", ErrUntrusted, err)
	}
	return (*windows.CertInfo)(unsafe.Pointer(&buf[0])), nil
}

func cryptMsgGetParam(msg windows.Handle, paramType uint32, index uint32, data *byte, size *uint32) error {
	r, _, err := procCryptMsgGetParam.Call(
		uintptr(msg),
		uintptr(paramType),
		uintptr(index),
		uintptr(unsafe.Pointer(data)),
		uintptr(unsafe.Pointer(size)),
	)
	if r == 0 {
		return err
	}
	return nil
}

func cryptMsgClose(msg windows.Handle) {
	_, _, _ = procCryptMsgClose.Call(uintptr(msg))
}

// isHRESULT reports whether err is the given HRESULT. WinVerifyTrust returns
// its result as an errno-shaped value, so the comparison goes through
// syscall.Errno rather than a windows.Handle-typed constant.
func isHRESULT(err error, hresult uint32) bool {
	var errno syscall.Errno
	return errors.As(err, &errno) && uint32(errno) == hresult
}

// describeTrustError turns the HRESULTs worth acting on into something a
// person reading the log can use, and passes everything else through.
func describeTrustError(err error) string {
	switch {
	case isHRESULT(err, trustBadDigest):
		return "the file does not match its signature (tampered or truncated)"
	case isHRESULT(err, trustNoSignature):
		return "the file is not signed"
	case isHRESULT(err, certUntrustedRoot):
		return "the signature does not chain to the expected certificate"
	default:
		return err.Error()
	}
}

// subtleEqual compares two thumbprints in constant time. They are public
// values, so this is not load-bearing; it just keeps a security comparison
// from being written the way that eventually gets copied somewhere it matters.
func subtleEqual(a, b []byte) bool {
	var diff byte
	for i := range a {
		diff |= a[i] ^ b[i]
	}
	return diff == 0
}

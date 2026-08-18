// Package cert installs the 3gIT code-signing certificate into the Windows
// trust stores, so EMLy and its setup elevate as a verified publisher rather
// than "Unknown publisher".
//
// The certificate is embedded rather than fetched from the update API. A
// trust anchor pulled off the network has to be pinned to a thumbprint
// compiled into the binary anyway - otherwise anyone able to intercept the
// (plain-HTTP by default) internal source could have their own certificate
// trusted fleet-wide. Once the thumbprint has to ship in the binary,
// embedding the 778-byte certificate itself costs nothing and removes the
// endpoint, the network failure mode, and the interception risk outright.
//
// The trade-off accepted: rotating the certificate requires a new release.
package cert

import (
	"crypto/x509"
	_ "embed"
	"fmt"
)

//go:embed 3GITInnovation.cer
var embedded []byte

// Embedded parses and returns the embedded 3gIT code-signing certificate
// together with its raw DER encoding. The DER is what the Windows store APIs
// consume; the parsed certificate is for inspection and logging.
func Embedded() (*x509.Certificate, []byte, error) {
	c, err := x509.ParseCertificate(embedded)
	if err != nil {
		return nil, nil, fmt.Errorf("embedded certificate is not valid DER: %w", err)
	}
	return c, embedded, nil
}

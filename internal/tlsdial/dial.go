// Package tlsdial upgrades an existing TCP connection to a TLSv1.0 session
// speaking the exact cipher and renegotiation flags the AST2300 BMC expects.
//
// # Security
//
// The BMC ships a self-signed Avocent certificate that expired in 2020. It
// cannot be verified against any real CA, and pretending it can would be
// theatre. Instead this package requires certificate pinning: the caller
// supplies the expected SHA-256 fingerprint of the BMC's leaf certificate
// (produced by `PinFromServer` on first use and stored under
// `testdata/bmc-cert.pin`). Any deviation, including a genuine MITM, breaks
// the pin and aborts the connection.
//
// # ClientHello shape
//
// Go's crypto/tls sends a modern ClientHello with `supported_versions` and
// key-share extensions. The AST2300 rejects it with "illegal parameter".
// Only openssl 1.x-style minimal Hellos survive. We craft one via utls,
// carrying just the cipher, the null compression method, ec_point_formats,
// and the TLS_EMPTY_RENEGOTIATION_INFO_SCSV signalling cipher used by
// pre-RFC5746 stacks.
package tlsdial

import (
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"

	utls "github.com/refraction-networking/utls"
)

// bmcCipherSuites are the cipher IDs the BMC's TLS stack expects to see
// offered before it will agree to a handshake. Even though our target cipher
// is AES256-SHA (0x0035, the one the BMC actually selects), the AST2300
// disconnects with "handshake_failure" when we offer fewer than ~9 ciphers.
// Copied from openssl's default TLSv1.0 hello observed against this BMC.
var bmcCipherSuites = []uint16{
	0xc00a, // TLS_ECDHE_ECDSA_WITH_AES_256_CBC_SHA
	0xc014, // TLS_ECDHE_RSA_WITH_AES_256_CBC_SHA
	0x0039, // TLS_DHE_RSA_WITH_AES_256_CBC_SHA
	0xc009, // TLS_ECDHE_ECDSA_WITH_AES_128_CBC_SHA
	0xc013, // TLS_ECDHE_RSA_WITH_AES_128_CBC_SHA
	0x0033, // TLS_DHE_RSA_WITH_AES_128_CBC_SHA
	0x0035, // TLS_RSA_WITH_AES_256_CBC_SHA   ← selected by BMC
	0x002f, // TLS_RSA_WITH_AES_128_CBC_SHA
	0x00ff, // TLS_EMPTY_RENEGOTIATION_INFO_SCSV
}

// FingerprintSHA256 returns the lowercase hex SHA-256 of the DER form of a
// certificate.
func FingerprintSHA256(cert *x509.Certificate) string {
	sum := sha256.Sum256(cert.Raw)
	return hex.EncodeToString(sum[:])
}

// PinFromServer opens a one-shot TLS session and returns the leaf-cert
// SHA-256 fingerprint. The raw connection is consumed; the caller closes it.
func PinFromServer(raw net.Conn, serverName string) (string, error) {
	c, err := handshake(raw, serverName, "")
	if err != nil {
		return "", err
	}
	// Do not close c: closing it will close raw, but the caller passed us
	// raw and wants to keep managing it. Instead just read out the cert.
	certs := c.ConnectionState().PeerCertificates
	if len(certs) == 0 {
		return "", errors.New("tlsdial: peer sent no certificate")
	}
	return FingerprintSHA256(certs[0]), nil
}

// lastLeaf holds the most recent BMC leaf certificate seen during a handshake,
// captured so the app can display cert details (subject/issuer/expiry) without
// opening a second connection — the BMC resets extra TLS sessions on the KVM
// port and its HTTPS port speaks a stack Go/openssl can't cleanly negotiate.
var (
	lastLeafMu sync.Mutex
	lastLeaf   *x509.Certificate
)

// LastLeaf returns the BMC leaf certificate captured during the live session's
// TLS handshake, or nil if no handshake has completed yet.
func LastLeaf() *x509.Certificate {
	lastLeafMu.Lock()
	defer lastLeafMu.Unlock()
	return lastLeaf
}

// UpgradeToTLS wraps raw with a TLSv1.0 session pinned to expectedFingerprint.
// serverName is unused on the wire (SNI is omitted for BMC compatibility) but
// kept in the signature for symmetry with other TLS helpers.
func UpgradeToTLS(raw net.Conn, serverName, expectedFingerprint string) (net.Conn, error) {
	c, err := handshake(raw, serverName, expectedFingerprint)
	if err != nil {
		return nil, err
	}
	return c, nil
}

func handshake(raw net.Conn, serverName, expectedFingerprint string) (*utls.UConn, error) {
	cfg := &utls.Config{
		MinVersion:         utls.VersionTLS10,
		MaxVersion:         utls.VersionTLS10,
		CipherSuites:       bmcCipherSuites,
		InsecureSkipVerify: true,
		Renegotiation:      utls.RenegotiateFreelyAsClient,
		VerifyPeerCertificate: func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
			if len(rawCerts) == 0 {
				if expectedFingerprint == "" {
					return nil
				}
				return errors.New("tlsdial: peer sent no certificate")
			}
			leaf, err := x509.ParseCertificate(rawCerts[0])
			if err != nil {
				if expectedFingerprint == "" {
					return nil
				}
				return fmt.Errorf("tlsdial: parse leaf: %w", err)
			}
			// Capture the leaf so the app can display cert details later
			// without opening a second (rejected) connection to the BMC.
			lastLeafMu.Lock()
			lastLeaf = leaf
			lastLeafMu.Unlock()
			if expectedFingerprint == "" {
				return nil
			}
			got := FingerprintSHA256(leaf)
			want := strings.ToLower(strings.ReplaceAll(expectedFingerprint, ":", ""))
			if got != want {
				return fmt.Errorf("tlsdial: certificate pin mismatch\n  expected %s\n  got      %s", want, got)
			}
			return nil
		},
	}
	c := utls.UClient(raw, cfg, utls.HelloCustom)
	// A near-minimal TLSv1.0 ClientHello mimicking openssl's output.
	// Ordering and constants come from a hex dump of `openssl s_client
	// -tls1 -cipher DEFAULT@SECLEVEL=0 -legacy_renegotiation` against
	// this BMC. No SNI (the BMC dislikes IP-only names).
	spec := &utls.ClientHelloSpec{
		TLSVersMin:         utls.VersionTLS10,
		TLSVersMax:         utls.VersionTLS10,
		CipherSuites:       bmcCipherSuites,
		CompressionMethods: []byte{0}, // null compression
		Extensions: []utls.TLSExtension{
			&utls.SupportedPointsExtension{SupportedPoints: []byte{0}}, // uncompressed
			&utls.SupportedCurvesExtension{Curves: []utls.CurveID{29, 23, 30, 24, 25}},
		},
	}
	if err := c.ApplyPreset(spec); err != nil {
		return nil, fmt.Errorf("tlsdial: apply spec: %w", err)
	}
	if err := c.Handshake(); err != nil {
		return nil, fmt.Errorf("tlsdial: handshake to %s: %w", serverName, err)
	}
	return c, nil
}

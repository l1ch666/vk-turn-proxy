// Package dtlsauth contains the shared DTLS identity verification policy.
package dtlsauth

import (
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/pion/dtls/v3"
)

const fingerprintBytes = sha256.Size

type authenticationMode uint8

const (
	authenticationUnset authenticationMode = iota
	authenticationPinned
	authenticationInsecure
)

// Fingerprint is the SHA-256 digest of a DER-encoded leaf certificate.
type Fingerprint [fingerprintBytes]byte

// ClientAuthentication describes how the client authenticates the DTLS
// server. Its zero value is deliberately invalid so omitted configuration
// cannot silently disable peer verification.
type ClientAuthentication struct {
	mode     authenticationMode
	expected Fingerprint
}

// ParseFingerprint accepts either 64 hexadecimal characters or the standard
// 32 colon-separated hexadecimal byte pairs.
func ParseFingerprint(value string) (Fingerprint, error) {
	var fingerprint Fingerprint
	value = strings.TrimSpace(value)
	if value == "" {
		return fingerprint, fmt.Errorf("DTLS certificate fingerprint must not be empty")
	}

	normalized := value
	if strings.Contains(value, ":") {
		parts := strings.Split(value, ":")
		if len(parts) != fingerprintBytes {
			return fingerprint, fmt.Errorf("DTLS certificate fingerprint must contain %d byte pairs", fingerprintBytes)
		}
		for _, part := range parts {
			if len(part) != 2 {
				return fingerprint, fmt.Errorf("DTLS certificate fingerprint byte pairs must contain two hexadecimal characters")
			}
		}
		normalized = strings.Join(parts, "")
	}
	if len(normalized) != fingerprintBytes*2 {
		return fingerprint, fmt.Errorf("DTLS certificate fingerprint must contain %d hexadecimal characters", fingerprintBytes*2)
	}
	decoded, err := hex.DecodeString(normalized)
	if err != nil {
		return fingerprint, fmt.Errorf("decode DTLS certificate fingerprint: %w", err)
	}
	copy(fingerprint[:], decoded)
	return fingerprint, nil
}

// CertificateFingerprint returns the SHA-256 fingerprint of the leaf
// certificate presented by a tls.Certificate.
func CertificateFingerprint(certificate tls.Certificate) (Fingerprint, error) {
	if len(certificate.Certificate) == 0 || len(certificate.Certificate[0]) == 0 {
		return Fingerprint{}, fmt.Errorf("DTLS certificate has no leaf certificate")
	}
	return sha256.Sum256(certificate.Certificate[0]), nil
}

// NewClientAuthentication validates the fail-closed client authentication
// policy. A pin and the insecure override are mutually exclusive.
func NewClientAuthentication(fingerprint string, insecureSkipVerify bool) (ClientAuthentication, error) {
	fingerprint = strings.TrimSpace(fingerprint)
	if insecureSkipVerify {
		if fingerprint != "" {
			return ClientAuthentication{}, fmt.Errorf("DTLS fingerprint and insecure skip-verify cannot be enabled together")
		}
		return ClientAuthentication{mode: authenticationInsecure}, nil
	}
	if fingerprint == "" {
		return ClientAuthentication{}, fmt.Errorf("DTLS server fingerprint is required; set -dtls-server-fingerprint or explicitly opt in to -dtls-insecure-skip-verify")
	}
	expected, err := ParseFingerprint(fingerprint)
	if err != nil {
		return ClientAuthentication{}, err
	}
	return ClientAuthentication{mode: authenticationPinned, expected: expected}, nil
}

// Options returns the Pion DTLS options that enforce this authentication
// policy. Certificate-chain verification is intentionally bypassed only
// because the exact leaf certificate digest is checked instead.
func (authentication ClientAuthentication) Options() ([]dtls.ClientOption, error) {
	switch authentication.mode {
	case authenticationPinned:
		return []dtls.ClientOption{
			dtls.WithInsecureSkipVerify(true),
			dtls.WithVerifyPeerCertificate(verifyPinnedCertificate(authentication.expected)),
		}, nil
	case authenticationInsecure:
		return []dtls.ClientOption{dtls.WithInsecureSkipVerify(true)}, nil
	default:
		return nil, fmt.Errorf("DTLS client authentication is not configured")
	}
}

// Description is suitable for startup logs and contains no secret material.
func (authentication ClientAuthentication) Description() string {
	switch authentication.mode {
	case authenticationPinned:
		return "pinned SHA-256 certificate " + authentication.expected.String()
	case authenticationInsecure:
		return "disabled by explicit insecure override"
	default:
		return "not configured"
	}
}

// Insecure reports whether peer authentication was explicitly disabled.
func (authentication ClientAuthentication) Insecure() bool {
	return authentication.mode == authenticationInsecure
}

func verifyPinnedCertificate(expected Fingerprint) func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
	return func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
		return verifyRawCertificateFingerprint(expected, rawCerts)
	}
}

func (fingerprint Fingerprint) String() string {
	encoded := strings.ToUpper(hex.EncodeToString(fingerprint[:]))
	pairs := make([]string, 0, fingerprintBytes)
	for offset := 0; offset < len(encoded); offset += 2 {
		pairs = append(pairs, encoded[offset:offset+2])
	}
	return strings.Join(pairs, ":")
}

func verifyRawCertificateFingerprint(expected Fingerprint, rawCerts [][]byte) error {
	if len(rawCerts) == 0 || len(rawCerts[0]) == 0 {
		return fmt.Errorf("DTLS peer did not present a certificate")
	}
	actual := sha256.Sum256(rawCerts[0])
	if subtle.ConstantTimeCompare(expected[:], actual[:]) != 1 {
		return fmt.Errorf("DTLS server certificate fingerprint mismatch: got %s", Fingerprint(actual).String())
	}
	return nil
}

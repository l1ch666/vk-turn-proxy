package dtlsauth

import (
	"crypto/tls"
	"strings"
	"testing"
)

const testFingerprintPlain = "000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f"

func TestParseFingerprintFormats(t *testing.T) {
	want := Fingerprint{
		0x00, 0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07,
		0x08, 0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f,
		0x10, 0x11, 0x12, 0x13, 0x14, 0x15, 0x16, 0x17,
		0x18, 0x19, 0x1a, 0x1b, 0x1c, 0x1d, 0x1e, 0x1f,
	}
	for _, value := range []string{testFingerprintPlain, want.String(), "  " + strings.ToUpper(testFingerprintPlain) + "  "} {
		got, err := ParseFingerprint(value)
		if err != nil {
			t.Fatalf("parse %q: %v", value, err)
		}
		if got != want {
			t.Fatalf("parse %q = %v, want %v", value, got, want)
		}
	}
}

func TestParseFingerprintRejectsMalformedValues(t *testing.T) {
	for _, value := range []string{"", "00", strings.Repeat("gg", 32), strings.Repeat("00:", 31) + "0", strings.Repeat("00:", 32) + "00"} {
		if _, err := ParseFingerprint(value); err == nil {
			t.Fatalf("malformed fingerprint %q was accepted", value)
		}
	}
}

func TestClientAuthenticationFailsClosed(t *testing.T) {
	if _, err := NewClientAuthentication("", false); err == nil {
		t.Fatal("missing fingerprint was accepted")
	}
	if _, err := NewClientAuthentication(testFingerprintPlain, true); err == nil {
		t.Fatal("pin plus insecure override was accepted")
	}
	var zero ClientAuthentication
	if _, err := zero.Options(); err == nil {
		t.Fatal("zero authentication policy produced DTLS options")
	}
}

func TestClientAuthenticationModes(t *testing.T) {
	pinned, err := NewClientAuthentication(testFingerprintPlain, false)
	if err != nil {
		t.Fatal(err)
	}
	if pinned.Insecure() || !strings.Contains(pinned.Description(), "pinned SHA-256") {
		t.Fatalf("unexpected pinned policy: %s", pinned.Description())
	}
	if options, err := pinned.Options(); err != nil || len(options) != 2 {
		t.Fatalf("pinned options = %d, %v", len(options), err)
	}

	insecure, err := NewClientAuthentication("", true)
	if err != nil {
		t.Fatal(err)
	}
	if !insecure.Insecure() || !strings.Contains(insecure.Description(), "insecure") {
		t.Fatalf("unexpected insecure policy: %s", insecure.Description())
	}
	if options, err := insecure.Options(); err != nil || len(options) != 1 {
		t.Fatalf("insecure options = %d, %v", len(options), err)
	}
}

func TestCertificateFingerprintAndVerifier(t *testing.T) {
	certificate := tls.Certificate{Certificate: [][]byte{[]byte("leaf certificate")}}
	expected, err := CertificateFingerprint(certificate)
	if err != nil {
		t.Fatal(err)
	}
	if err := verifyRawCertificateFingerprint(expected, certificate.Certificate); err != nil {
		t.Fatalf("matching certificate rejected: %v", err)
	}
	if err := verifyRawCertificateFingerprint(expected, [][]byte{[]byte("different certificate")}); err == nil {
		t.Fatal("different certificate was accepted")
	}
	if err := verifyRawCertificateFingerprint(expected, nil); err == nil {
		t.Fatal("missing certificate was accepted")
	}
	if _, err := CertificateFingerprint(tls.Certificate{}); err == nil {
		t.Fatal("empty certificate produced a fingerprint")
	}
}

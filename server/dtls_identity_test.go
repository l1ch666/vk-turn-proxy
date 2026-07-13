package main

import (
	"crypto/ed25519"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"

	"github.com/l1ch666/vk-turn-proxy/dtlsauth"
	"github.com/pion/dtls/v3"
	"github.com/pion/dtls/v3/pkg/crypto/selfsign"
)

func TestLoadServerDTLSIdentityRequiresCertificateAndKeyTogether(t *testing.T) {
	if _, err := loadServerDTLSIdentity("certificate.pem", ""); err == nil {
		t.Fatal("certificate without key was accepted")
	}
	if _, err := loadServerDTLSIdentity("", "key.pem"); err == nil {
		t.Fatal("key without certificate was accepted")
	}
}

func TestLoadServerDTLSIdentityGeneratesEphemeralIdentity(t *testing.T) {
	identity, err := loadServerDTLSIdentity("", "")
	if err != nil {
		t.Fatal(err)
	}
	if !identity.ephemeral {
		t.Fatal("generated identity was not marked ephemeral")
	}
	if identity.cipherSuite != dtls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256 {
		t.Fatalf("generated identity cipher = %v", identity.cipherSuite)
	}
	want, err := dtlsauth.CertificateFingerprint(identity.certificate)
	if err != nil {
		t.Fatal(err)
	}
	if identity.fingerprint != want {
		t.Fatalf("generated identity fingerprint = %s, want %s", identity.fingerprint, want)
	}
}

func TestLoadServerDTLSIdentityLoadsPersistentPEM(t *testing.T) {
	certificate, err := selfsign.GenerateSelfSigned()
	if err != nil {
		t.Fatal(err)
	}
	privateKey, err := x509.MarshalPKCS8PrivateKey(certificate.PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	certificateFile := filepath.Join(directory, "server-cert.pem")
	keyFile := filepath.Join(directory, "server-key.pem")
	if err := os.WriteFile(certificateFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificate.Certificate[0]}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privateKey}), 0o600); err != nil {
		t.Fatal(err)
	}

	identity, err := loadServerDTLSIdentity(certificateFile, keyFile)
	if err != nil {
		t.Fatal(err)
	}
	if identity.ephemeral {
		t.Fatal("loaded identity was marked ephemeral")
	}
	want, err := dtlsauth.CertificateFingerprint(certificate)
	if err != nil {
		t.Fatal(err)
	}
	if identity.fingerprint != want {
		t.Fatalf("loaded identity fingerprint = %s, want %s", identity.fingerprint, want)
	}
}

func TestLoadServerDTLSIdentityRejectsInvalidPEM(t *testing.T) {
	directory := t.TempDir()
	certificateFile := filepath.Join(directory, "server-cert.pem")
	keyFile := filepath.Join(directory, "server-key.pem")
	if err := os.WriteFile(certificateFile, []byte("not a certificate"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, []byte("not a key"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadServerDTLSIdentity(certificateFile, keyFile); err == nil {
		t.Fatal("invalid PEM pair was accepted")
	}
}

func TestCertificateCipherSuiteMatchesPublicKey(t *testing.T) {
	for _, test := range []struct {
		name    string
		key     any
		want    dtls.CipherSuiteID
		wantErr bool
	}{
		{name: "rsa", key: &rsa.PublicKey{}, want: dtls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256},
		{name: "unsupported ed25519", key: ed25519.PublicKey{1}, wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			certificate := tls.Certificate{
				Certificate: [][]byte{{1}},
				Leaf:        &x509.Certificate{PublicKey: test.key},
			}
			got, err := certificateCipherSuite(certificate)
			if test.wantErr {
				if err == nil {
					t.Fatalf("unsupported key %T was accepted", test.key)
				}
				return
			}
			if err != nil || got != test.want {
				t.Fatalf("cipher suite = %v, %v; want %v", got, err, test.want)
			}
		})
	}
}

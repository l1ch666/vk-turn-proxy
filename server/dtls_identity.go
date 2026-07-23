package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/l1ch666/vk-turn-proxy/v2/dtlsauth"
	"github.com/pion/dtls/v3"
	"github.com/pion/dtls/v3/pkg/crypto/selfsign"
)

type serverDTLSIdentity struct {
	certificate tls.Certificate
	fingerprint dtlsauth.Fingerprint
	cipherSuite dtls.CipherSuiteID
	ephemeral   bool
}

func loadServerDTLSIdentity(certificateFile, keyFile string) (serverDTLSIdentity, error) {
	certificateSet := strings.TrimSpace(certificateFile) != ""
	keySet := strings.TrimSpace(keyFile) != ""
	if certificateSet != keySet {
		return serverDTLSIdentity{}, fmt.Errorf("-dtls-cert-file and -dtls-key-file must be provided together")
	}

	var (
		certificate tls.Certificate
		ephemeral   bool
		err         error
	)
	if certificateSet {
		if sameIdentityPath(certificateFile, keyFile) {
			if _, statErr := os.Stat(certificateFile); errors.Is(statErr, os.ErrNotExist) {
				if err := createPersistentDTLSIdentity(certificateFile); err != nil {
					return serverDTLSIdentity{}, err
				}
			} else if statErr != nil {
				return serverDTLSIdentity{}, fmt.Errorf("stat DTLS identity file: %w", statErr)
			}
		}
		if err := validatePrivateIdentityFile(keyFile); err != nil {
			return serverDTLSIdentity{}, err
		}
		certificate, err = tls.LoadX509KeyPair(certificateFile, keyFile)
		if err != nil {
			return serverDTLSIdentity{}, fmt.Errorf("load DTLS certificate and key: %w", err)
		}
	} else {
		certificate, err = selfsign.GenerateSelfSigned()
		if err != nil {
			return serverDTLSIdentity{}, fmt.Errorf("generate ephemeral DTLS certificate: %w", err)
		}
		ephemeral = true
	}
	if certificate.Leaf == nil && len(certificate.Certificate) > 0 {
		certificate.Leaf, err = x509.ParseCertificate(certificate.Certificate[0])
		if err != nil {
			return serverDTLSIdentity{}, fmt.Errorf("parse DTLS leaf certificate: %w", err)
		}
	}

	fingerprint, err := dtlsauth.CertificateFingerprint(certificate)
	if err != nil {
		return serverDTLSIdentity{}, err
	}
	cipherSuite, err := certificateCipherSuite(certificate)
	if err != nil {
		return serverDTLSIdentity{}, err
	}
	return serverDTLSIdentity{
		certificate: certificate,
		fingerprint: fingerprint,
		cipherSuite: cipherSuite,
		ephemeral:   ephemeral,
	}, nil
}

func validatePrivateIdentityFile(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("stat DTLS private key: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("DTLS private key path must not be a symbolic link")
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("DTLS private key path is not a regular file")
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("DTLS private key permissions %04o are too broad; use 0600", info.Mode().Perm())
	}
	return nil
}

func sameIdentityPath(certificateFile, keyFile string) bool {
	certificatePath := filepath.Clean(certificateFile)
	keyPath := filepath.Clean(keyFile)
	if runtime.GOOS == "windows" {
		return strings.EqualFold(certificatePath, keyPath)
	}
	return certificatePath == keyPath
}

func createPersistentDTLSIdentity(path string) error {
	certificate, err := generatePersistentDTLSCertificate()
	if err != nil {
		return fmt.Errorf("generate persistent DTLS certificate: %w", err)
	}
	if len(certificate.Certificate) == 0 {
		return fmt.Errorf("generated DTLS certificate has no leaf certificate")
	}
	privateKey, err := x509.MarshalPKCS8PrivateKey(certificate.PrivateKey)
	if err != nil {
		return fmt.Errorf("marshal persistent DTLS private key: %w", err)
	}
	encoded := append(
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificate.Certificate[0]}),
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privateKey})...,
	)

	dir := filepath.Dir(path)
	file, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create temporary DTLS identity: %w", err)
	}
	tempPath := file.Name()
	defer func() { _ = os.Remove(tempPath) }()
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return fmt.Errorf("set DTLS identity permissions: %w", err)
	}
	writeErr := func() error {
		if _, err := file.Write(encoded); err != nil {
			return err
		}
		return file.Sync()
	}()
	closeErr := file.Close()
	if writeErr != nil {
		return fmt.Errorf("write DTLS identity: %w", writeErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close DTLS identity: %w", closeErr)
	}
	if err := os.Link(tempPath, path); err != nil && !errors.Is(err, os.ErrExist) {
		return fmt.Errorf("publish DTLS identity atomically: %w", err)
	}
	return nil
}

func generatePersistentDTLSCertificate() (tls.Certificate, error) {
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	serialLimit := new(big.Int).Lsh(big.NewInt(1), 128)
	serial, err := rand.Int(rand.Reader, serialLimit)
	if err != nil {
		return tls.Certificate{}, err
	}
	now := time.Now()
	template := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName: "vk-turn-proxy",
		},
		NotBefore:             now.Add(-5 * time.Minute),
		NotAfter:              now.AddDate(10, 0, 0),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &privateKey.PublicKey, privateKey)
	if err != nil {
		return tls.Certificate{}, err
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return tls.Certificate{}, err
	}
	return tls.Certificate{
		Certificate: [][]byte{der},
		PrivateKey:  privateKey,
		Leaf:        leaf,
	}, nil
}

func certificateCipherSuite(certificate tls.Certificate) (dtls.CipherSuiteID, error) {
	if len(certificate.Certificate) == 0 {
		return 0, fmt.Errorf("DTLS certificate has no leaf certificate")
	}
	leaf := certificate.Leaf
	if leaf == nil {
		var err error
		leaf, err = x509.ParseCertificate(certificate.Certificate[0])
		if err != nil {
			return 0, fmt.Errorf("parse DTLS leaf certificate: %w", err)
		}
	}
	switch leaf.PublicKey.(type) {
	case *ecdsa.PublicKey:
		return dtls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256, nil
	case *rsa.PublicKey:
		return dtls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256, nil
	default:
		return 0, fmt.Errorf("unsupported DTLS certificate public key type %T; use ECDSA or RSA", leaf.PublicKey)
	}
}

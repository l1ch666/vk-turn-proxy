package main

import (
	"crypto/ecdsa"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"strings"

	"github.com/l1ch666/vk-turn-proxy/dtlsauth"
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

package main

import (
	"crypto/tls"

	"github.com/l1ch666/vk-turn-proxy/dtlsauth"
	"github.com/pion/dtls/v3"
)

func clientDTLSOptions(certificate tls.Certificate, authentication dtlsauth.ClientAuthentication) ([]dtls.ClientOption, error) {
	authenticationOptions, err := authentication.Options()
	if err != nil {
		return nil, err
	}
	options := []dtls.ClientOption{
		dtls.WithCertificates(certificate),
		dtls.WithExtendedMasterSecret(dtls.RequireExtendedMasterSecret),
		dtls.WithCipherSuites(
			dtls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256,
			dtls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,
		),
		dtls.WithConnectionIDGenerator(dtls.OnlySendCIDGenerator()),
	}
	return append(options, authenticationOptions...), nil
}

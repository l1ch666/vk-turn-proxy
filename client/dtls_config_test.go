package main

import (
	"testing"

	"github.com/l1ch666/vk-turn-proxy/dtlsauth"
	"github.com/pion/dtls/v3/pkg/crypto/selfsign"
)

func TestClientDTLSOptionsRequireAuthentication(t *testing.T) {
	certificate, err := selfsign.GenerateSelfSigned()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := clientDTLSOptions(certificate, dtlsauth.ClientAuthentication{}); err == nil {
		t.Fatal("zero DTLS authentication produced client options")
	}
}

func TestClientDTLSOptionsIncludeAuthenticationPolicy(t *testing.T) {
	certificate, err := selfsign.GenerateSelfSigned()
	if err != nil {
		t.Fatal(err)
	}
	pinned, err := dtlsauth.NewClientAuthentication("000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f", false)
	if err != nil {
		t.Fatal(err)
	}
	options, err := clientDTLSOptions(certificate, pinned)
	if err != nil {
		t.Fatal(err)
	}
	if len(options) != 6 {
		t.Fatalf("client DTLS options = %d, want 6", len(options))
	}
}

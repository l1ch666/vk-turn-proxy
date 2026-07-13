package main

import (
	"context"
	"crypto/tls"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/l1ch666/vk-turn-proxy/dtlsauth"
	"github.com/pion/dtls/v3"
	"github.com/pion/dtls/v3/pkg/crypto/selfsign"
)

func testPinnedDTLSHandshake(t *testing.T, certificate tls.Certificate, authentication dtlsauth.ClientAuthentication) (error, error) {
	t.Helper()
	listener, err := dtls.ListenWithOptions(
		"udp",
		&net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)},
		dtls.WithCertificates(certificate),
		dtls.WithExtendedMasterSecret(dtls.RequireExtendedMasterSecret),
		dtls.WithCipherSuites(dtls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256),
		dtls.WithConnectionIDGenerator(dtls.RandomCIDGenerator(8)),
	)
	if err != nil {
		t.Fatalf("listen DTLS: %v", err)
	}
	defer func() { _ = listener.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	serverResult := make(chan error, 1)
	go func() {
		connection, acceptErr := listener.Accept()
		if acceptErr != nil {
			serverResult <- acceptErr
			return
		}
		defer func() { _ = connection.Close() }()
		dtlsConnection, ok := connection.(*dtls.Conn)
		if !ok {
			serverResult <- nil
			return
		}
		serverResult <- dtlsConnection.HandshakeContext(ctx)
	}()

	packetConn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen client UDP: %v", err)
	}
	peer, ok := listener.Addr().(*net.UDPAddr)
	if !ok {
		_ = packetConn.Close()
		t.Fatalf("DTLS listener address has type %T", listener.Addr())
	}
	clientConnection, clientErr := dtlsFunc(ctx, packetConn, peer, authentication)
	if clientConnection != nil {
		_ = clientConnection.Close()
	} else {
		_ = packetConn.Close()
	}
	serverErr := <-serverResult
	return clientErr, serverErr
}

func TestDTLSHandshakeAcceptsPinnedServerCertificate(t *testing.T) {
	certificate, err := selfsign.GenerateSelfSigned()
	if err != nil {
		t.Fatal(err)
	}
	fingerprint, err := dtlsauth.CertificateFingerprint(certificate)
	if err != nil {
		t.Fatal(err)
	}
	authentication, err := dtlsauth.NewClientAuthentication(fingerprint.String(), false)
	if err != nil {
		t.Fatal(err)
	}
	clientErr, serverErr := testPinnedDTLSHandshake(t, certificate, authentication)
	if clientErr != nil || serverErr != nil {
		t.Fatalf("pinned handshake failed: client=%v server=%v", clientErr, serverErr)
	}
}

func TestDTLSHandshakeRejectsDifferentServerCertificate(t *testing.T) {
	certificate, err := selfsign.GenerateSelfSigned()
	if err != nil {
		t.Fatal(err)
	}
	otherCertificate, err := selfsign.GenerateSelfSigned()
	if err != nil {
		t.Fatal(err)
	}
	otherFingerprint, err := dtlsauth.CertificateFingerprint(otherCertificate)
	if err != nil {
		t.Fatal(err)
	}
	authentication, err := dtlsauth.NewClientAuthentication(otherFingerprint.String(), false)
	if err != nil {
		t.Fatal(err)
	}
	clientErr, _ := testPinnedDTLSHandshake(t, certificate, authentication)
	if clientErr == nil || !strings.Contains(clientErr.Error(), "fingerprint mismatch") {
		t.Fatalf("unexpected client error for mismatched pin: %v", clientErr)
	}
}

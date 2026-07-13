package main

import (
	"context"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cbeuw/connutil"
	"github.com/l1ch666/vk-turn-proxy/dtlsauth"
)

type failingHandshakePacketConn struct {
	closed atomic.Bool
}

func (c *failingHandshakePacketConn) ReadFrom([]byte) (int, net.Addr, error) {
	return 0, nil, io.EOF
}

func (c *failingHandshakePacketConn) WriteTo(p []byte, _ net.Addr) (int, error) {
	return len(p), nil
}

func (c *failingHandshakePacketConn) Close() error {
	c.closed.Store(true)
	return nil
}

func (c *failingHandshakePacketConn) LocalAddr() net.Addr {
	return &net.UDPAddr{IP: net.IPv4zero}
}

func (c *failingHandshakePacketConn) SetDeadline(time.Time) error      { return nil }
func (c *failingHandshakePacketConn) SetReadDeadline(time.Time) error  { return nil }
func (c *failingHandshakePacketConn) SetWriteDeadline(time.Time) error { return nil }

func TestDTLSFuncClosesTransportAfterHandshakeFailure(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	packetConn := &failingHandshakePacketConn{}
	peer := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 443}
	authentication, err := dtlsauth.NewClientAuthentication("", true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := dtlsFunc(ctx, packetConn, peer, authentication); err == nil {
		t.Fatal("expected DTLS handshake failure")
	}
	if !packetConn.closed.Load() {
		t.Fatal("DTLS transport was not closed after handshake failure")
	}
}

type signalingPacketConn struct {
	net.PacketConn
	readStarted chan struct{}
	readDone    chan struct{}
	startedOnce sync.Once
	doneOnce    sync.Once
}

func (c *signalingPacketConn) ReadFrom(p []byte) (int, net.Addr, error) {
	c.startedOnce.Do(func() { close(c.readStarted) })
	n, addr, err := c.PacketConn.ReadFrom(p)
	c.doneOnce.Do(func() { close(c.readDone) })
	return n, addr, err
}

func waitForLifecycleSignal(t *testing.T, signal <-chan struct{}, name string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for %s", name)
	}
}

func TestBridgeTURNPacketsStopsReaderBeforeReconnect(t *testing.T) {
	dtlsSide, bridgeSide := connutil.AsyncPacketPipe()
	t.Cleanup(func() { _ = dtlsSide.Close() })

	oldRelay, oldRelayPeer := connutil.AsyncPacketPipe()
	t.Cleanup(func() { _ = oldRelayPeer.Close() })
	monitoredPipe := &signalingPacketConn{
		PacketConn:  bridgeSide,
		readStarted: make(chan struct{}),
		readDone:    make(chan struct{}),
	}

	firstCtx, firstCancel := context.WithCancel(context.Background())
	firstDone := make(chan struct{})
	go func() {
		bridgeTURNPackets(firstCtx, oldRelay, monitoredPipe, &net.UDPAddr{}, 1)
		close(firstDone)
	}()
	waitForLifecycleSignal(t, monitoredPipe.readStarted, "first pipe reader to start")

	if err := oldRelayPeer.Close(); err != nil {
		t.Fatalf("failed to stop first relay: %v", err)
	}
	waitForLifecycleSignal(t, firstDone, "first bridge to stop")
	waitForLifecycleSignal(t, monitoredPipe.readDone, "first pipe reader to stop")
	firstCancel()

	newRelay, newRelayPeer := connutil.AsyncPacketPipe()
	t.Cleanup(func() { _ = newRelayPeer.Close() })
	secondCtx, secondCancel := context.WithCancel(context.Background())
	secondDone := make(chan struct{})
	go func() {
		bridgeTURNPackets(secondCtx, newRelay, bridgeSide, &net.UDPAddr{}, 2)
		close(secondDone)
	}()

	if _, err := dtlsSide.WriteTo([]byte("ping"), nil); err != nil {
		t.Fatalf("failed to write packet for reconnected bridge: %v", err)
	}
	if err := newRelayPeer.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("failed to set relay read deadline: %v", err)
	}
	buf := make([]byte, 16)
	n, _, err := newRelayPeer.ReadFrom(buf)
	if err != nil {
		t.Fatalf("reconnected bridge did not receive packet: %v", err)
	}
	if got := string(buf[:n]); got != "ping" {
		t.Fatalf("reconnected bridge received %q, want ping", got)
	}

	secondCancel()
	waitForLifecycleSignal(t, secondDone, "second bridge to stop")
}

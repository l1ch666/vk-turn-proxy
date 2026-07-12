package main

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/cacggghp/vk-turn-proxy/tcputil"
	"github.com/xtaci/smux"
)

func TestPipeConnReturnsWhenOneDirectionEnds(t *testing.T) {
	left1, right1 := net.Pipe()
	left2, right2 := net.Pipe()
	t.Cleanup(func() {
		_ = left1.Close()
		_ = right1.Close()
		_ = left2.Close()
		_ = right2.Close()
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		pipeConn(ctx, left1, left2)
		close(done)
	}()

	if err := right1.Close(); err != nil {
		t.Fatalf("failed to close one pipe direction: %v", err)
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("pipeConn did not stop after one direction closed")
	}
}

func TestServeSmuxSessionReturnsOnContextCancel(t *testing.T) {
	serverConn, clientConn := net.Pipe()
	serverSession, err := smux.Server(serverConn, tcputil.DefaultSmuxConfig())
	if err != nil {
		t.Fatalf("failed to create smux server: %v", err)
	}
	clientSession, err := smux.Client(clientConn, tcputil.DefaultSmuxConfig())
	if err != nil {
		_ = serverSession.Close()
		t.Fatalf("failed to create smux client: %v", err)
	}
	t.Cleanup(func() {
		_ = clientSession.Close()
		_ = serverSession.Close()
		_ = clientConn.Close()
		_ = serverConn.Close()
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		serveSmuxSession(ctx, serverSession, "127.0.0.1:1")
		close(done)
	}()
	cancel()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("serveSmuxSession did not stop after context cancellation")
	}
}

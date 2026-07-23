package tcputil

import (
	"context"
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

type blockingCloseWriteConn struct {
	net.Conn
	closeOnce sync.Once
	closed    chan struct{}
}

func newBlockingCloseWriteConn(conn net.Conn) *blockingCloseWriteConn {
	return &blockingCloseWriteConn{Conn: conn, closed: make(chan struct{})}
}

func (conn *blockingCloseWriteConn) CloseWrite() error {
	<-conn.closed
	return net.ErrClosed
}

func (conn *blockingCloseWriteConn) Close() error {
	conn.closeOnce.Do(func() { close(conn.closed) })
	return conn.Conn.Close()
}

func tcpPair(t *testing.T) (*net.TCPConn, *net.TCPConn) {
	t.Helper()
	listener, err := net.ListenTCP("tcp", &net.TCPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	accepted := make(chan *net.TCPConn, 1)
	acceptErr := make(chan error, 1)
	go func() {
		conn, acceptError := listener.AcceptTCP()
		if acceptError != nil {
			acceptErr <- acceptError
			return
		}
		accepted <- conn
	}()

	client, err := net.DialTCP("tcp", nil, listener.Addr().(*net.TCPAddr))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	select {
	case server := <-accepted:
		return client, server
	case err := <-acceptErr:
		_ = client.Close()
		t.Fatalf("accept: %v", err)
	case <-time.After(time.Second):
		_ = client.Close()
		t.Fatal("accept timed out")
	}
	return nil, nil
}

func TestPipePreservesHalfCloseResponse(t *testing.T) {
	leftApp, leftProxy := tcpPair(t)
	rightProxy, rightApp := tcpPair(t)
	for _, conn := range []*net.TCPConn{leftApp, leftProxy, rightProxy, rightApp} {
		conn := conn
		t.Cleanup(func() { _ = conn.Close() })
	}

	done := make(chan error, 1)
	go func() {
		done <- Pipe(context.Background(), leftProxy, rightProxy)
	}()

	if _, err := leftApp.Write([]byte("request")); err != nil {
		t.Fatalf("write request: %v", err)
	}
	if err := leftApp.CloseWrite(); err != nil {
		t.Fatalf("half-close request: %v", err)
	}
	request, err := io.ReadAll(rightApp)
	if err != nil {
		t.Fatalf("read request: %v", err)
	}
	if string(request) != "request" {
		t.Fatalf("request = %q, want %q", request, "request")
	}

	if _, err := rightApp.Write([]byte("response")); err != nil {
		t.Fatalf("write response: %v", err)
	}
	if err := rightApp.CloseWrite(); err != nil {
		t.Fatalf("half-close response: %v", err)
	}
	response, err := io.ReadAll(leftApp)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	if string(response) != "response" {
		t.Fatalf("response = %q, want %q", response, "response")
	}

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Pipe returned an error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Pipe did not finish after both half-closes")
	}
}

func TestPipeStopsOnContextCancellation(t *testing.T) {
	left, leftPeer := net.Pipe()
	right, rightPeer := net.Pipe()
	for _, conn := range []net.Conn{left, leftPeer, right, rightPeer} {
		conn := conn
		t.Cleanup(func() { _ = conn.Close() })
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Pipe(ctx, left, right)
	}()
	cancel()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Pipe returned nil after context cancellation")
		}
	case <-time.After(time.Second):
		t.Fatal("Pipe did not stop after context cancellation")
	}
}

func TestPipeBoundsBlockingCloseWrite(t *testing.T) {
	leftBase, leftPeer := net.Pipe()
	right, rightPeer := net.Pipe()
	left := newBlockingCloseWriteConn(leftBase)
	for _, conn := range []net.Conn{left, leftPeer, right, rightPeer} {
		conn := conn
		t.Cleanup(func() { _ = conn.Close() })
	}

	done := make(chan error, 1)
	go func() {
		done <- pipeWithHalfCloseTimeout(
			context.Background(),
			left,
			right,
			40*time.Millisecond,
		)
	}()
	// EOF on right triggers the deliberately blocking half-close on left.
	if err := rightPeer.Close(); err != nil {
		t.Fatalf("close right peer: %v", err)
	}

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Pipe returned nil after a half-close timeout")
		}
	case <-time.After(time.Second):
		t.Fatal("Pipe remained blocked in CloseWrite")
	}
	select {
	case <-left.closed:
	default:
		t.Fatal("timed-out half-close did not fully close the destination")
	}
}

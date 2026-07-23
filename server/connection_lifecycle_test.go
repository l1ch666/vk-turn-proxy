package main

import (
	"context"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/l1ch666/vk-turn-proxy/v2/metrics"
	"github.com/l1ch666/vk-turn-proxy/v2/tcputil"
	"github.com/xtaci/smux"
)

type recordingKCPWindow struct {
	mu      sync.Mutex
	windows []int
}

func newLifecycleTestBackendGate(t *testing.T) *backendGate {
	t.Helper()
	gate, err := newBackendGate(16, 8, &metrics.Registry{})
	if err != nil {
		t.Fatalf("create backend gate: %v", err)
	}
	return gate
}

func (r *recordingKCPWindow) SetWindowSize(send, receive int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if send != receive {
		panic("test received asymmetric KCP window")
	}
	r.windows = append(r.windows, send)
}

func (r *recordingKCPWindow) last() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.windows) == 0 {
		return 0
	}
	return r.windows[len(r.windows)-1]
}

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
	gate := newLifecycleTestBackendGate(t)
	done := make(chan struct{})
	go func() {
		serveSmuxSession(ctx, serverSession, "127.0.0.1:1", gate)
		close(done)
	}()
	cancel()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("serveSmuxSession did not stop after context cancellation")
	}
}

func TestServeSmuxSessionRejectsStreamsOverBackendLimit(t *testing.T) {
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
	backendListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen for backend: %v", err)
	}
	t.Cleanup(func() {
		_ = backendListener.Close()
		_ = clientSession.Close()
		_ = serverSession.Close()
		_ = clientConn.Close()
		_ = serverConn.Close()
	})

	accepted := make(chan net.Conn, 1)
	acceptErr := make(chan error, 1)
	go func() {
		conn, acceptError := backendListener.Accept()
		if acceptError != nil {
			acceptErr <- acceptError
			return
		}
		accepted <- conn
	}()

	registry := &metrics.Registry{}
	gate, err := newBackendGate(1, 1, registry)
	if err != nil {
		t.Fatalf("create backend gate: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	serveDone := make(chan struct{})
	go func() {
		serveSmuxSession(ctx, serverSession, backendListener.Addr().String(), gate)
		close(serveDone)
	}()

	first, err := clientSession.OpenStream()
	if err != nil {
		t.Fatalf("open first stream: %v", err)
	}
	defer func() { _ = first.Close() }()
	if _, err := first.Write([]byte{1}); err != nil {
		t.Fatalf("write first stream: %v", err)
	}
	var backendConn net.Conn
	select {
	case backendConn = <-accepted:
		defer func() { _ = backendConn.Close() }()
	case err := <-acceptErr:
		t.Fatalf("accept backend connection: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("first stream did not open a backend connection")
	}
	if got := registry.Snapshot().ActiveBackendStreams; got != 1 {
		t.Fatalf("active backend streams = %d, want 1", got)
	}

	second, err := clientSession.OpenStream()
	if err != nil {
		t.Fatalf("open second stream: %v", err)
	}
	defer func() { _ = second.Close() }()
	_, _ = second.Write([]byte{2})
	deadline := time.Now().Add(2 * time.Second)
	for registry.Snapshot().BackendLimitRejections != 1 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := registry.Snapshot().BackendLimitRejections; got != 1 {
		t.Fatalf("backend limit rejections = %d, want 1", got)
	}
	if err := second.SetReadDeadline(time.Now().Add(time.Second)); err == nil {
		if _, err := second.Read(make([]byte, 1)); err == nil {
			t.Fatal("rejected stream remained readable")
		}
	}

	cancel()
	select {
	case <-serveDone:
	case <-time.After(2 * time.Second):
		t.Fatal("serveSmuxSession did not stop after cancellation")
	}
	if got := registry.Snapshot().ActiveBackendStreams; got != 0 {
		t.Fatalf("active backend streams after shutdown = %d, want 0", got)
	}
}

func TestHandleVLESSConnectionReturnsWhileWaitingForKCP(t *testing.T) {
	serverConn, peerConn := net.Pipe()
	t.Cleanup(func() {
		_ = serverConn.Close()
		_ = peerConn.Close()
	})

	ctx, cancel := context.WithCancel(context.Background())
	gate := newLifecycleTestBackendGate(t)
	done := make(chan struct{})
	go func() {
		handleVLESSConnection(ctx, serverConn, "127.0.0.1:1", gate)
		close(done)
	}()
	cancel()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("handleVLESSConnection did not stop while waiting for KCP")
	}
}

func TestVLESSBondGroupReturnsWhileWaitingForKCP(t *testing.T) {
	group := &vlessBondGroup{
		id:          "0123456789abcdef",
		connectAddr: "127.0.0.1:1",
		pc:          tcputil.NewBondedPacketConn("test-shutdown"),
	}
	t.Cleanup(func() { _ = group.pc.Close() })

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		group.run(ctx, func() {})
		close(done)
	}()
	cancel()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("VLESS bond group did not stop while waiting for KCP")
	}
}

func TestVLESSBondManagerWaitsForGroupShutdown(t *testing.T) {
	manager := newVLESSBondManager("127.0.0.1:1")
	serverConn, peerConn := net.Pipe()
	t.Cleanup(func() {
		_ = serverConn.Close()
		_ = peerConn.Close()
	})

	writeDone := make(chan error, 1)
	go func() {
		writeDone <- tcputil.WriteBondHello(peerConn, "0123456789abcdef")
	}()
	ctx, cancel := context.WithCancel(context.Background())
	if err := manager.Add(ctx, serverConn); err != nil {
		cancel()
		t.Fatalf("failed to add bond path: %v", err)
	}
	if err := <-writeDone; err != nil {
		cancel()
		t.Fatalf("failed to write bond hello: %v", err)
	}

	cancel()
	done := make(chan struct{})
	go func() {
		manager.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("VLESS bond manager did not wait for group shutdown")
	}

	manager.mu.Lock()
	defer manager.mu.Unlock()
	if len(manager.groups) != 0 {
		t.Fatalf("bond manager retained %d stopped groups", len(manager.groups))
	}
}

func TestVLESSBondManagerRunsPathCleanupExactlyOnce(t *testing.T) {
	manager := newVLESSBondManager("127.0.0.1:1")
	serverConn, peerConn := net.Pipe()
	t.Cleanup(func() {
		_ = serverConn.Close()
		_ = peerConn.Close()
	})

	writeDone := make(chan error, 1)
	go func() {
		writeDone <- tcputil.WriteBondHello(peerConn, "0123456789abcdef")
	}()
	ctx, cancel := context.WithCancel(context.Background())
	var cleanupCalls atomic.Int32
	if err := manager.AddWithCleanup(ctx, serverConn, func() { cleanupCalls.Add(1) }); err != nil {
		cancel()
		t.Fatalf("failed to add bond path: %v", err)
	}
	if err := <-writeDone; err != nil {
		cancel()
		t.Fatalf("failed to write bond hello: %v", err)
	}

	cancel()
	manager.Wait()
	_ = serverConn.Close()
	if got := cleanupCalls.Load(); got != 1 {
		t.Fatalf("path cleanup calls = %d, want 1", got)
	}
}

func TestVLESSBondManagerAddsFirstPathBeforeStartingGroup(t *testing.T) {
	manager := newVLESSBondManager("127.0.0.1:1")
	startedWith := make(chan int, 1)
	manager.runGroup = func(ctx context.Context, group *vlessBondGroup, onDone func()) {
		startedWith <- group.pc.Count()
		<-ctx.Done()
		_ = group.pc.Close()
		onDone()
	}

	serverConn, peerConn := net.Pipe()
	t.Cleanup(func() {
		_ = serverConn.Close()
		_ = peerConn.Close()
	})
	writeDone := make(chan error, 1)
	go func() { writeDone <- tcputil.WriteBondHello(peerConn, "0123456789abcdef") }()

	ctx, cancel := context.WithCancel(context.Background())
	if err := manager.Add(ctx, serverConn); err != nil {
		cancel()
		t.Fatalf("failed to add bond path: %v", err)
	}
	if err := <-writeDone; err != nil {
		cancel()
		t.Fatalf("failed to write bond hello: %v", err)
	}
	select {
	case count := <-startedWith:
		if count < 1 {
			t.Fatalf("group started with %d paths, want at least 1", count)
		}
	case <-time.After(time.Second):
		cancel()
		t.Fatal("group did not start")
	}
	cancel()
	manager.Wait()
}

func TestVLESSBondGroupGrowsKCPWindowForNewPaths(t *testing.T) {
	group := &vlessBondGroup{
		id:          "0123456789abcdef",
		connectAddr: "127.0.0.1:1",
		pc:          tcputil.NewBondedPacketConn("test-window-growth"),
	}
	t.Cleanup(func() { _ = group.pc.Close() })

	recorder := &recordingKCPWindow{}
	_, initialWindow := group.installWindowSetter(recorder)
	if initialWindow != tcputil.BondedKCPWindow(1) {
		t.Fatalf("initial window = %d, want %d", initialWindow, tcputil.BondedKCPWindow(1))
	}

	path1, peer1 := net.Pipe()
	path2, peer2 := net.Pipe()
	t.Cleanup(func() {
		_ = peer1.Close()
		_ = peer2.Close()
	})
	if err := group.add(path1); err != nil {
		t.Fatalf("first path rejected: %v", err)
	}
	if err := group.add(path2); err != nil {
		t.Fatalf("second path rejected: %v", err)
	}

	want := tcputil.BondedKCPWindow(2)
	if got := recorder.last(); got != want {
		t.Fatalf("KCP window after two paths = %d, want %d", got, want)
	}
}

func TestVLESSBondGroupUsesNegotiatedPathCount(t *testing.T) {
	hello := tcputil.CurrentBondHello("0123456789abcdef", 10)
	group := &vlessBondGroup{
		id:          hello.BondID,
		connectAddr: "127.0.0.1:1",
		pc:          tcputil.NewBondedPacketConn("test-negotiated-window"),
		hello:       hello,
	}
	t.Cleanup(func() { _ = group.pc.Close() })

	recorder := &recordingKCPWindow{}
	pathCount, window := group.installWindowSetter(recorder)
	if pathCount != 10 {
		t.Fatalf("negotiated path count = %d, want 10", pathCount)
	}
	if want := tcputil.BondedKCPWindow(10); window != want || recorder.last() != want {
		t.Fatalf("negotiated window = %d/%d, want %d", window, recorder.last(), want)
	}
}

func TestVLESSBondGroupRejectsExtraNegotiatedPath(t *testing.T) {
	hello := tcputil.CurrentBondHello("0123456789abcdef", 1)
	group := &vlessBondGroup{
		id:          hello.BondID,
		connectAddr: "127.0.0.1:1",
		pc:          tcputil.NewBondedPacketConn("test-path-limit"),
		hello:       hello,
	}
	t.Cleanup(func() { _ = group.pc.Close() })

	path1, peer1 := net.Pipe()
	path2, peer2 := net.Pipe()
	t.Cleanup(func() {
		_ = peer1.Close()
		_ = peer2.Close()
		_ = path2.Close()
	})
	if err := group.add(path1); err != nil {
		t.Fatalf("first path rejected: %v", err)
	}
	if err := group.add(path2); err == nil {
		t.Fatal("extra negotiated path unexpectedly accepted")
	}
}

func TestVLESSBondManagerAcceptsV2AndAcknowledges(t *testing.T) {
	manager := newVLESSBondManager("127.0.0.1:1")
	serverConn, clientConn := net.Pipe()
	t.Cleanup(func() {
		_ = serverConn.Close()
		_ = clientConn.Close()
	})

	hello := tcputil.CurrentBondHello("0123456789abcdef", 2)
	clientDone := make(chan error, 1)
	go func() {
		if err := tcputil.WriteBondHelloConfig(clientConn, hello); err != nil {
			clientDone <- err
			return
		}
		clientDone <- tcputil.ReadBondHelloAck(clientConn)
	}()

	ctx, cancel := context.WithCancel(context.Background())
	if err := manager.Add(ctx, serverConn); err != nil {
		cancel()
		t.Fatalf("V2 path rejected: %v", err)
	}
	if err := <-clientDone; err != nil {
		cancel()
		t.Fatalf("V2 client handshake failed: %v", err)
	}
	cancel()
	manager.Wait()
}

func TestVLESSBondManagerRejectsMismatchedV2ProfileWithNACK(t *testing.T) {
	manager := newVLESSBondManager("127.0.0.1:1")
	serverConn, clientConn := net.Pipe()
	t.Cleanup(func() {
		_ = serverConn.Close()
		_ = clientConn.Close()
	})

	hello := tcputil.CurrentBondHello("0123456789abcdef", 2)
	if hello.MTU > 50 {
		hello.MTU--
	} else {
		hello.MTU++
	}
	clientDone := make(chan error, 1)
	go func() {
		if err := tcputil.WriteBondHelloConfig(clientConn, hello); err != nil {
			clientDone <- err
			return
		}
		clientDone <- tcputil.ReadBondHelloAck(clientConn)
	}()

	err := manager.Add(context.Background(), serverConn)
	if err == nil {
		t.Fatal("mismatched V2 profile unexpectedly accepted")
	}
	if ackErr := <-clientDone; !errors.Is(ackErr, tcputil.ErrBondHelloRejected) {
		t.Fatalf("client response error = %v, want explicit V2 rejection", ackErr)
	}
}

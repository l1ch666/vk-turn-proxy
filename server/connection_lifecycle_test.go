package main

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/cacggghp/vk-turn-proxy/tcputil"
	"github.com/xtaci/smux"
)

type recordingKCPWindow struct {
	mu      sync.Mutex
	windows []int
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

func TestHandleVLESSConnectionReturnsWhileWaitingForKCP(t *testing.T) {
	serverConn, peerConn := net.Pipe()
	t.Cleanup(func() {
		_ = serverConn.Close()
		_ = peerConn.Close()
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		handleVLESSConnection(ctx, serverConn, "127.0.0.1:1")
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
	group.add(path1)
	group.add(path2)

	want := tcputil.BondedKCPWindow(2)
	if got := recorder.last(); got != want {
		t.Fatalf("KCP window after two paths = %d, want %d", got, want)
	}
}

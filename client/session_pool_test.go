package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/xtaci/smux"
)

type failingPooledSession struct {
	err error
}

func (session failingPooledSession) OpenStream() (*smux.Stream, error) {
	return nil, session.err
}

func (failingPooledSession) IsClosed() bool  { return false }
func (failingPooledSession) NumStreams() int { return 0 }
func (failingPooledSession) Close() error    { return nil }

type blockingPooledSession struct {
	closeOnce sync.Once
	closed    chan struct{}
}

func newBlockingPooledSession() *blockingPooledSession {
	return &blockingPooledSession{closed: make(chan struct{})}
}

func (session *blockingPooledSession) OpenStream() (*smux.Stream, error) {
	<-session.closed
	return nil, net.ErrClosed
}

func (session *blockingPooledSession) IsClosed() bool {
	select {
	case <-session.closed:
		return true
	default:
		return false
	}
}

func (*blockingPooledSession) NumStreams() int { return 0 }

func (session *blockingPooledSession) Close() error {
	session.closeOnce.Do(func() { close(session.closed) })
	return nil
}

// newSmuxPair returns a connected client/server smux session pair over an
// in-memory pipe, plus a cleanup func.
func newSmuxPair(t *testing.T) (client *smux.Session, server *smux.Session, cleanup func()) {
	t.Helper()
	c, s := net.Pipe()

	type res struct {
		sess *smux.Session
		err  error
	}
	srvCh := make(chan res, 1)
	go func() {
		sess, err := smux.Server(s, smux.DefaultConfig())
		srvCh <- res{sess, err}
	}()

	client, err := smux.Client(c, smux.DefaultConfig())
	if err != nil {
		t.Fatalf("smux client: %v", err)
	}
	sr := <-srvCh
	if sr.err != nil {
		t.Fatalf("smux server: %v", sr.err)
	}
	server = sr.sess

	cleanup = func() {
		_ = client.Close()
		_ = server.Close()
		_ = c.Close()
		_ = s.Close()
	}
	return client, server, cleanup
}

func TestPickLeastLoadedEmptyPool(t *testing.T) {
	p := &sessionPool{}
	if got := p.pickLeastLoaded(); got != nil {
		t.Fatalf("expected nil from empty pool, got %v", got)
	}
}

func TestPickLeastLoadedPrefersFewestStreams(t *testing.T) {
	cBusy, sBusy, cleanBusy := newSmuxPair(t)
	defer cleanBusy()
	cIdle, sIdle, cleanIdle := newSmuxPair(t)
	defer cleanIdle()
	_ = sIdle

	// Accept and consume one byte from each stream so every client write is
	// acknowledged before the server side can close. Keep all streams open
	// until the pool assertions finish so NumStreams remains deterministic.
	acceptResult := make(chan error, 1)
	releaseAccepted := make(chan struct{})
	serverDone := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseAccepted) }) }
	defer release()
	go func() {
		defer close(serverDone)
		accepted := make([]*smux.Stream, 0, 3)
		defer func() {
			for _, stream := range accepted {
				_ = stream.Close()
			}
		}()
		for i := 0; i < 3; i++ {
			st, err := sBusy.AcceptStream()
			if err != nil {
				acceptResult <- fmt.Errorf("accept stream %d: %w", i, err)
				return
			}
			accepted = append(accepted, st)
			var payload [1]byte
			if _, err := io.ReadFull(st, payload[:]); err != nil {
				acceptResult <- fmt.Errorf("read stream %d: %w", i, err)
				return
			}
		}
		acceptResult <- nil
		<-releaseAccepted
	}()

	for i := 0; i < 3; i++ {
		st, err := cBusy.OpenStream()
		if err != nil {
			t.Fatalf("open stream %d: %v", i, err)
		}
		// Write a byte so the server's AcceptStream returns and the stream is
		// fully established/counted on both sides.
		if _, err := st.Write([]byte{0}); err != nil {
			t.Fatalf("write stream %d: %v", i, err)
		}
		defer func() { _ = st.Close() }()
	}
	if err := <-acceptResult; err != nil {
		t.Fatal(err)
	}

	if cBusy.NumStreams() == 0 {
		t.Fatalf("busy session should report streams, got 0")
	}
	if cIdle.NumStreams() != 0 {
		t.Fatalf("idle session should have 0 streams, got %d", cIdle.NumStreams())
	}

	p := &sessionPool{}
	p.add(cBusy)
	p.add(cIdle)

	// Across several picks the idle session must always win (0 < busy load).
	for i := 0; i < 8; i++ {
		if got := p.pickLeastLoaded(); got != cIdle {
			t.Fatalf("pick %d: expected idle session, got busy", i)
		}
	}
	release()
	<-serverDone
}

func TestPickLeastLoadedSkipsClosed(t *testing.T) {
	cClosed, _, cleanClosed := newSmuxPair(t)
	defer cleanClosed()
	cLive, _, cleanLive := newSmuxPair(t)
	defer cleanLive()

	_ = cClosed.Close() // mark closed

	p := &sessionPool{}
	p.add(cClosed)
	p.add(cLive)

	for i := 0; i < 5; i++ {
		got := p.pickLeastLoaded()
		if got != cLive {
			t.Fatalf("pick %d: expected the live session, got the closed one", i)
		}
	}
}

func TestOpenStreamTriesAnotherSessionAfterFailure(t *testing.T) {
	client, server, cleanup := newSmuxPair(t)
	defer cleanup()
	accepted := make(chan *smux.Stream, 1)
	go func() {
		stream, err := server.AcceptStream()
		if err == nil {
			accepted <- stream
		}
	}()

	pool := &sessionPool{}
	pool.add(failingPooledSession{err: errors.New("stalled path")})
	pool.add(client)
	stream, err := pool.openStream(context.Background(), time.Second)
	if err != nil {
		t.Fatalf("openStream did not fail over: %v", err)
	}
	defer func() { _ = stream.Close() }()
	select {
	case acceptedStream := <-accepted:
		_ = acceptedStream.Close()
	case <-time.After(time.Second):
		t.Fatal("fallback session did not receive the stream")
	}
}

func TestOpenStreamBoundsStalledSessionAndFailsOver(t *testing.T) {
	client, server, cleanup := newSmuxPair(t)
	defer cleanup()
	accepted := make(chan *smux.Stream, 1)
	go func() {
		stream, err := server.AcceptStream()
		if err == nil {
			accepted <- stream
		}
	}()

	stalled := newBlockingPooledSession()
	pool := &sessionPool{}
	pool.add(client)
	pool.add(stalled)

	started := time.Now()
	stream, err := pool.openStreamWithAttemptTimeout(
		context.Background(),
		500*time.Millisecond,
		40*time.Millisecond,
	)
	if err != nil {
		t.Fatalf("openStream did not fail over from stalled session: %v", err)
	}
	defer func() { _ = stream.Close() }()
	if elapsed := time.Since(started); elapsed > 300*time.Millisecond {
		t.Fatalf("stalled session failover took %s, want at most 300ms", elapsed)
	}
	if !stalled.IsClosed() {
		t.Fatal("stalled session was not closed after attempt timeout")
	}

	select {
	case acceptedStream := <-accepted:
		_ = acceptedStream.Close()
	case <-time.After(time.Second):
		t.Fatal("fallback session did not receive the stream")
	}
}

func TestOpenStreamWaitsBrieflyForSession(t *testing.T) {
	client, _, cleanup := newSmuxPair(t)
	defer cleanup()
	pool := &sessionPool{}
	go func() {
		time.Sleep(30 * time.Millisecond)
		pool.add(client)
	}()
	stream, err := pool.openStream(context.Background(), time.Second)
	if err != nil {
		t.Fatalf("openStream did not wait for a session: %v", err)
	}
	_ = stream.Close()
}

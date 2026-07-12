package main

import (
	"context"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cacggghp/vk-turn-proxy/tcputil"
	"github.com/xtaci/smux"
)

type fakeBondSmuxSession struct {
	closed chan struct{}
	once   sync.Once
}

func newFakeBondSmuxSession() *fakeBondSmuxSession {
	return &fakeBondSmuxSession{closed: make(chan struct{})}
}

func (s *fakeBondSmuxSession) OpenStream() (*smux.Stream, error) {
	return nil, fmt.Errorf("fake session does not open streams")
}

func (s *fakeBondSmuxSession) IsClosed() bool {
	select {
	case <-s.closed:
		return true
	default:
		return false
	}
}

func (s *fakeBondSmuxSession) CloseChan() <-chan struct{} {
	return s.closed
}

func (s *fakeBondSmuxSession) Close() error {
	s.once.Do(func() { close(s.closed) })
	return nil
}

func TestSuperviseVLESSBondReplacesFailedGenerationAfterCleanup(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	slot := newVLESSBondSessionSlot()
	created := make(chan *vlessBondGeneration, 3)
	overlapped := make(chan bool, 1)

	var mu sync.Mutex
	factoryCalls := 0
	firstCleaned := false
	cleanupCalls := make(map[string]int)
	factory := func(context.Context) (*vlessBondGeneration, error) {
		mu.Lock()
		factoryCalls++
		call := factoryCalls
		if call == 2 {
			overlapped <- !firstCleaned
		}
		mu.Unlock()

		id := fmt.Sprintf("generation-%d", call)
		session := newFakeBondSmuxSession()
		generation := newVLESSBondGeneration(id, session, nil, func() {
			_ = session.Close()
			mu.Lock()
			cleanupCalls[id]++
			if call == 1 {
				firstCleaned = true
			}
			mu.Unlock()
		})
		created <- generation
		return generation, nil
	}

	done := make(chan struct{})
	go func() {
		superviseVLESSBond(ctx, slot, factory, func(int) time.Duration { return time.Millisecond })
		close(done)
	}()

	first := receiveGeneration(t, created)
	first.fail(fmt.Errorf("forced failure"))
	second := receiveGeneration(t, created)
	if second.id == first.id {
		t.Fatalf("generation id was not replaced: %s", second.id)
	}
	select {
	case overlap := <-overlapped:
		if overlap {
			t.Fatal("second generation started before first cleanup completed")
		}
	case <-time.After(time.Second):
		t.Fatal("second factory call was not observed")
	}
	waitForPublishedGeneration(t, slot, second)

	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("supervisor did not stop after cancellation")
	}
	if got := slot.get(); got != nil {
		t.Fatalf("slot retained generation %s after shutdown", got.id)
	}
	mu.Lock()
	defer mu.Unlock()
	if cleanupCalls[first.id] != 1 || cleanupCalls[second.id] != 1 {
		t.Fatalf("cleanup calls = %v, want each generation exactly once", cleanupCalls)
	}
}

func TestVLESSBondGenerationFailsWhenLastPathDisappears(t *testing.T) {
	bonded := tcputil.NewBondedPacketConn("test-generation-health")
	defer func() { _ = bonded.Close() }()
	left, right := net.Pipe()
	done := bonded.AddConn(left, nil)
	select {
	case <-bonded.StateChanged():
	case <-time.After(time.Second):
		t.Fatal("path add was not observed")
	}

	session := newFakeBondSmuxSession()
	generation := newVLESSBondGeneration("0123456789abcdef", session, bonded, func() { _ = session.Close() })
	waitDone := make(chan error, 1)
	go func() { waitDone <- generation.wait(context.Background()) }()
	if err := right.Close(); err != nil {
		t.Fatalf("close path peer: %v", err)
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("bond path did not stop")
	}
	select {
	case err := <-waitDone:
		if err == nil || !strings.Contains(err.Error(), "no active paths") {
			t.Fatalf("generation wait error = %v, want no-active-paths error", err)
		}
	case <-time.After(time.Second):
		t.Fatal("generation did not fail after losing its last path")
	}
}

func TestVLESSBondSessionSlotDoesNotClearNewerGeneration(t *testing.T) {
	slot := newVLESSBondSessionSlot()
	first := newVLESSBondGeneration("first", newFakeBondSmuxSession(), nil, nil)
	second := newVLESSBondGeneration("second", newFakeBondSmuxSession(), nil, nil)
	slot.publish(first)
	slot.publish(second)
	slot.clear(first)
	if got := slot.get(); got != second {
		t.Fatalf("clearing stale generation removed current generation: got %p, want %p", got, second)
	}
}

func TestVLESSBondGenerationRetryDelayIsBounded(t *testing.T) {
	for _, attempt := range []int{0, 1, 5, 100} {
		delay := vlessBondGenerationRetryDelay(attempt)
		maxDelay := time.Second
		for i := 0; i < attempt && maxDelay < 30*time.Second; i++ {
			maxDelay *= 2
			if maxDelay > 30*time.Second {
				maxDelay = 30 * time.Second
			}
		}
		if delay < maxDelay/2 || delay > maxDelay {
			t.Fatalf("attempt %d delay = %s, want %s..%s", attempt, delay, maxDelay/2, maxDelay)
		}
	}
}

func receiveGeneration(t *testing.T, generations <-chan *vlessBondGeneration) *vlessBondGeneration {
	t.Helper()
	select {
	case generation := <-generations:
		return generation
	case <-time.After(time.Second):
		t.Fatal("generation was not created")
		return nil
	}
}

func waitForPublishedGeneration(t *testing.T, slot *vlessBondSessionSlot, want *vlessBondGeneration) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if slot.get() == want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("generation %p was not published; current = %p", want, slot.get())
}

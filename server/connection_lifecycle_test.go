package main

import (
	"context"
	"net"
	"testing"
	"time"
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

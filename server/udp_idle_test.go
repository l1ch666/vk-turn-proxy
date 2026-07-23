package main

import (
	"context"
	"testing"
	"time"
)

func TestUDPActivityExtendsIdleDeadline(t *testing.T) {
	activity := newUDPActivity()
	timeout := 80 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	done := make(chan bool, 1)
	go func() {
		done <- activity.waitUntilIdle(ctx, timeout)
	}()
	time.Sleep(timeout / 2)
	activity.touch()

	select {
	case <-done:
		t.Fatal("idle watchdog fired before the refreshed timeout")
	case <-time.After(timeout / 2):
	}
	select {
	case idle := <-done:
		if !idle {
			t.Fatal("idle watchdog stopped without detecting idleness")
		}
	case <-time.After(timeout):
		t.Fatal("idle watchdog did not fire after activity stopped")
	}
}

func TestUDPActivityStopsOnContext(t *testing.T) {
	activity := newUDPActivity()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan bool, 1)
	go func() {
		done <- activity.waitUntilIdle(ctx, time.Hour)
	}()
	cancel()
	select {
	case idle := <-done:
		if idle {
			t.Fatal("canceled watchdog reported idle timeout")
		}
	case <-time.After(time.Second):
		t.Fatal("idle watchdog did not stop after cancellation")
	}
}

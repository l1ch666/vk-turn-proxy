package main

import (
	"context"
	"sync/atomic"
	"time"
)

const (
	udpIdleTimeout      = 30 * time.Minute
	udpPacketBufferSize = 2048
)

// udpActivity tracks monotonic elapsed time without taking a timer lock on
// every packet. The watchdog recalculates its remaining delay only when its
// coarse timer fires.
type udpActivity struct {
	started time.Time
	last    atomic.Int64
}

func newUDPActivity() *udpActivity {
	activity := &udpActivity{started: time.Now()}
	activity.touch()
	return activity
}

func (activity *udpActivity) touch() {
	activity.last.Store(time.Since(activity.started).Nanoseconds())
}

func (activity *udpActivity) waitUntilIdle(ctx context.Context, timeout time.Duration) bool {
	for {
		idleFor := time.Since(activity.started) - time.Duration(activity.last.Load())
		remaining := timeout - idleFor
		if remaining <= 0 {
			return true
		}
		timer := time.NewTimer(remaining)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return false
		case <-timer.C:
		}
	}
}

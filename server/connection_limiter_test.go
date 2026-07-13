package main

import (
	"net"
	"sync"
	"testing"
)

func TestNewConnectionLimiterValidatesLimits(t *testing.T) {
	for _, limits := range [][2]int{{0, 1}, {1, 0}, {1, 2}, {maximumConfiguredConnectionLimit + 1, 1}} {
		if _, err := newConnectionLimiter(limits[0], limits[1]); err == nil {
			t.Fatalf("limits %v unexpectedly accepted", limits)
		}
	}
	if _, err := newConnectionLimiter(4, 2); err != nil {
		t.Fatalf("valid limits rejected: %v", err)
	}
}

func TestConnectionLimiterEnforcesGlobalAndPerIPLimits(t *testing.T) {
	limiter, err := newConnectionLimiter(3, 2)
	if err != nil {
		t.Fatal(err)
	}
	ip1 := &net.UDPAddr{IP: net.ParseIP("192.0.2.1"), Port: 1000}
	ip1OtherPort := &net.UDPAddr{IP: net.ParseIP("192.0.2.1"), Port: 2000}
	ip2 := &net.UDPAddr{IP: net.ParseIP("192.0.2.2"), Port: 1000}
	ip3 := &net.UDPAddr{IP: net.ParseIP("192.0.2.3"), Port: 1000}

	lease1, rejection := limiter.Acquire(ip1)
	if lease1 == nil || rejection != connectionAccepted {
		t.Fatalf("first connection rejected: %q", rejection)
	}
	lease2, rejection := limiter.Acquire(ip1OtherPort)
	if lease2 == nil || rejection != connectionAccepted {
		t.Fatalf("second connection rejected: %q", rejection)
	}
	if lease, rejection := limiter.Acquire(ip1); lease != nil || rejection != connectionRejectedPerIP {
		t.Fatalf("per-IP overflow = lease %v, rejection %q", lease, rejection)
	}
	lease3, rejection := limiter.Acquire(ip2)
	if lease3 == nil || rejection != connectionAccepted {
		t.Fatalf("third connection rejected: %q", rejection)
	}
	if lease, rejection := limiter.Acquire(ip3); lease != nil || rejection != connectionRejectedGlobal {
		t.Fatalf("global overflow = lease %v, rejection %q", lease, rejection)
	}

	if !lease1.Release() || lease1.Release() {
		t.Fatal("lease release was not idempotent")
	}
	lease4, rejection := limiter.Acquire(ip3)
	if lease4 == nil || rejection != connectionAccepted {
		t.Fatalf("released slot was not reusable: %q", rejection)
	}
	lease2.Release()
	lease3.Release()
	lease4.Release()
	if total, perIP := limiter.counts("192.0.2.1"); total != 0 || perIP != 0 {
		t.Fatalf("remaining counts = total %d, per-IP %d", total, perIP)
	}
}

func TestConnectionLimiterCanonicalizesIPAndRejectsInvalidAddress(t *testing.T) {
	limiter, err := newConnectionLimiter(2, 1)
	if err != nil {
		t.Fatal(err)
	}
	lease, rejection := limiter.Acquire(&net.TCPAddr{IP: net.ParseIP("::ffff:192.0.2.10"), Port: 1})
	if lease == nil || rejection != connectionAccepted {
		t.Fatalf("mapped IPv4 rejected: %q", rejection)
	}
	if next, rejection := limiter.Acquire(&net.UDPAddr{IP: net.ParseIP("192.0.2.10"), Port: 2}); next != nil || rejection != connectionRejectedPerIP {
		t.Fatalf("canonical per-IP limit bypassed: lease %v, rejection %q", next, rejection)
	}
	if next, rejection := limiter.Acquire(nil); next != nil || rejection != connectionRejectedInvalidIP {
		t.Fatalf("nil address = lease %v, rejection %q", next, rejection)
	}
	lease.Release()
}

func TestConnectionLimiterConcurrentCapacity(t *testing.T) {
	const capacity = 8
	const workers = 64
	limiter, err := newConnectionLimiter(capacity, capacity)
	if err != nil {
		t.Fatal(err)
	}
	remote := &net.UDPAddr{IP: net.ParseIP("192.0.2.20"), Port: 1000}
	start := make(chan struct{})
	release := make(chan struct{})
	attempted := make(chan struct{}, workers)
	acquired := make(chan *connectionLease, workers)
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			lease, _ := limiter.Acquire(remote)
			if lease != nil {
				acquired <- lease
			}
			attempted <- struct{}{}
			if lease != nil {
				<-release
				lease.Release()
			}
		}()
	}
	close(start)
	for range workers {
		<-attempted
	}
	if got := len(acquired); got != capacity {
		t.Fatalf("concurrent acquisitions = %d, want %d", got, capacity)
	}
	close(release)
	wg.Wait()
	if total, perIP := limiter.counts("192.0.2.20"); total != 0 || perIP != 0 {
		t.Fatalf("remaining counts = total %d, per-IP %d", total, perIP)
	}
}

func TestShouldLogResourceRejectionUsesPowersOfTwo(t *testing.T) {
	for _, count := range []uint64{1, 2, 4, 8, 1024} {
		if !shouldLogResourceRejection(count) {
			t.Fatalf("count %d should be logged", count)
		}
	}
	for _, count := range []uint64{0, 3, 5, 6, 1023} {
		if shouldLogResourceRejection(count) {
			t.Fatalf("count %d should be suppressed", count)
		}
	}
}

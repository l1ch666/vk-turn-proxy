package main

import (
	"sync"
	"testing"

	"github.com/l1ch666/vk-turn-proxy/v2/metrics"
)

func TestNewBackendGateValidatesLimits(t *testing.T) {
	registry := &metrics.Registry{}
	for _, test := range []struct {
		global, perSession int
		registry           *metrics.Registry
	}{
		{global: 0, perSession: 1, registry: registry},
		{global: 1, perSession: 0, registry: registry},
		{global: 1, perSession: 2, registry: registry},
		{global: maximumConfiguredBackendLimit + 1, perSession: 1, registry: registry},
		{global: 1, perSession: 1, registry: nil},
	} {
		if _, err := newBackendGate(test.global, test.perSession, test.registry); err == nil {
			t.Fatalf("limits %+v unexpectedly accepted", test)
		}
	}
	if _, err := newBackendGate(4, 2, registry); err != nil {
		t.Fatalf("valid backend limits rejected: %v", err)
	}
}

func TestBackendGateEnforcesGlobalAndPerSessionLimits(t *testing.T) {
	registry := &metrics.Registry{}
	gate, err := newBackendGate(2, 1, registry)
	if err != nil {
		t.Fatal(err)
	}
	session1 := gate.NewSessionLimiter()
	session2 := gate.NewSessionLimiter()
	session3 := gate.NewSessionLimiter()

	lease1, rejection, _ := gate.Acquire(session1)
	if lease1 == nil || rejection != backendAccepted {
		t.Fatalf("first backend stream rejected: %q", rejection)
	}
	if lease, rejection, count := gate.Acquire(session1); lease != nil || rejection != backendRejectedPerSession || count != 1 {
		t.Fatalf("per-session overflow = lease %v, rejection %q, count %d", lease, rejection, count)
	}
	lease2, rejection, _ := gate.Acquire(session2)
	if lease2 == nil || rejection != backendAccepted {
		t.Fatalf("second backend stream rejected: %q", rejection)
	}
	if lease, rejection, count := gate.Acquire(session3); lease != nil || rejection != backendRejectedGlobal || count != 2 {
		t.Fatalf("global overflow = lease %v, rejection %q, count %d", lease, rejection, count)
	}

	snapshot := registry.Snapshot()
	if snapshot.ActiveBackendStreams != 2 || snapshot.BackendLimitRejections != 2 {
		t.Fatalf("backend metrics = %+v", snapshot)
	}
	if !lease1.Release() || lease1.Release() {
		t.Fatal("backend lease release was not idempotent")
	}
	lease2.Release()
	if gate.global.Active() != 0 || session1.Active() != 0 || session2.Active() != 0 || registry.Snapshot().ActiveBackendStreams != 0 {
		t.Fatalf("backend slots were not fully released")
	}
}

func TestBackendGateConcurrentCapacity(t *testing.T) {
	const capacity = 8
	const workers = 64
	registry := &metrics.Registry{}
	gate, err := newBackendGate(capacity, capacity, registry)
	if err != nil {
		t.Fatal(err)
	}
	session := gate.NewSessionLimiter()
	start := make(chan struct{})
	release := make(chan struct{})
	attempted := make(chan struct{}, workers)
	acquired := make(chan *backendLease, workers)
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			lease, _, _ := gate.Acquire(session)
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
		t.Fatalf("concurrent backend acquisitions = %d, want %d", got, capacity)
	}
	close(release)
	wg.Wait()
	if gate.global.Active() != 0 || session.Active() != 0 || registry.Snapshot().ActiveBackendStreams != 0 {
		t.Fatal("backend concurrency slots leaked")
	}
}

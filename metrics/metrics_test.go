package metrics

import (
	"sync"
	"testing"
)

func TestRegistrySnapshot(t *testing.T) {
	var registry Registry
	registry.PathOpened()
	registry.PathOpened()
	registry.PathClosed()
	registry.SessionOpened()
	registry.PathReconnected()
	registry.SessionReconnected()
	registry.AuthFailed()
	registry.QueueDropped()

	got := registry.Snapshot()
	want := Snapshot{
		ActivePaths:       1,
		ActiveSessions:    1,
		PathReconnects:    1,
		SessionReconnects: 1,
		AuthFailures:      1,
		QueueDrops:        1,
	}
	if got != want {
		t.Fatalf("snapshot = %+v, want %+v", got, want)
	}
}

func TestRegistryConcurrentCounters(t *testing.T) {
	var registry Registry
	const workers = 16
	const iterations = 1000
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range iterations {
				registry.PathReconnected()
				registry.QueueDropped()
			}
		}()
	}
	wg.Wait()

	snapshot := registry.Snapshot()
	want := int64(workers * iterations)
	if snapshot.PathReconnects != want || snapshot.QueueDrops != want {
		t.Fatalf("concurrent snapshot = %+v, want reconnects/drops %d", snapshot, want)
	}
}

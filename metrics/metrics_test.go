package metrics

import (
	"errors"
	"sync"
	"testing"
	"time"
)

func TestRegistrySnapshot(t *testing.T) {
	var registry Registry
	path1 := registry.OpenPath("bond/path-1")
	path2 := registry.OpenPath("bond/path-2")
	path1.Close()
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
	if got.ActivePaths != want.ActivePaths ||
		got.ActiveSessions != want.ActiveSessions ||
		got.PathReconnects != want.PathReconnects ||
		got.SessionReconnects != want.SessionReconnects ||
		got.AuthFailures != want.AuthFailures ||
		got.QueueDrops != want.QueueDrops {
		t.Fatalf("snapshot = %+v, want %+v", got, want)
	}
	if len(got.Paths) != 2 || got.Paths[0].Active || !got.Paths[1].Active {
		t.Fatalf("path snapshots = %+v, want one closed and one active", got.Paths)
	}
	path2.Close()
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

func TestPathRecordsTrafficErrorsAndLatency(t *testing.T) {
	var registry Registry
	path := registry.OpenPath("bond/path-1")
	path.ObserveRead(120, nil)
	path.ObserveRead(7, errors.New("read failed"))
	path.ObserveWrite(time.Time{}, 80, nil)
	path.ObserveWrite(time.Time{}, 3, errors.New("write failed"))
	path.observeWriteLatency(2 * time.Millisecond)
	path.observeWriteLatency(5 * time.Millisecond)
	path.Close()
	path.Close()

	snapshot := registry.Snapshot()
	if snapshot.ActivePaths != 0 || len(snapshot.Paths) != 1 {
		t.Fatalf("snapshot = %+v, want one closed path", snapshot)
	}
	got := snapshot.Paths[0]
	if got.Active || got.BytesRead != 127 || got.BytesWritten != 83 ||
		got.ReadOperations != 2 || got.WriteOperations != 2 ||
		got.ReadErrors != 1 || got.WriteErrors != 1 ||
		got.WriteLatencySamples != 2 ||
		got.WriteLatencyTotalNanoseconds != uint64(7*time.Millisecond) ||
		got.WriteLatencyMaxNanoseconds != uint64(5*time.Millisecond) {
		t.Fatalf("path snapshot = %+v", got)
	}
}

func TestPathWriteLatencySamplingRate(t *testing.T) {
	var registry Registry
	path := registry.OpenPath("bond/path-1")
	defer path.Close()

	for i := 0; i < writeLatencySampleRate*2; i++ {
		started := path.BeginWrite()
		path.ObserveWrite(started, 1, nil)
	}

	got := registry.Snapshot().Paths[0]
	if got.WriteOperations != writeLatencySampleRate*2 || got.WriteLatencySamples != 2 {
		t.Fatalf("path snapshot = %+v, want %d writes and 2 latency samples", got, writeLatencySampleRate*2)
	}
}

func TestRegistryBoundsRecentPathHistory(t *testing.T) {
	var registry Registry
	for i := 0; i < recentPathLimit+10; i++ {
		registry.OpenPath("bond/path").Close()
	}

	snapshot := registry.Snapshot()
	if snapshot.ActivePaths != 0 || len(snapshot.Paths) != recentPathLimit {
		t.Fatalf("snapshot has %d active and %d retained paths, want 0 and %d", snapshot.ActivePaths, len(snapshot.Paths), recentPathLimit)
	}
	if snapshot.Paths[0].ID != 11 || snapshot.Paths[len(snapshot.Paths)-1].ID != recentPathLimit+10 {
		t.Fatalf("retained path IDs = %d..%d, want 11..%d", snapshot.Paths[0].ID, snapshot.Paths[len(snapshot.Paths)-1].ID, recentPathLimit+10)
	}
}

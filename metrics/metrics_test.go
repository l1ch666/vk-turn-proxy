package metrics

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/xtaci/kcp-go/v5"
)

func TestRegistrySnapshot(t *testing.T) {
	var registry Registry
	path1 := registry.OpenPath("bond/path-1")
	path2 := registry.OpenPath("bond/path-2")
	path1.Close()
	registry.TransportConnectionOpened()
	registry.BackendStreamOpened()
	registry.SessionOpened()
	registry.ConnectionLimitRejected()
	registry.BackendLimitRejected()
	registry.PathReconnected()
	registry.SessionReconnected()
	registry.AuthFailed()
	registry.QueueDropped()

	got := registry.Snapshot()
	want := Snapshot{
		ActiveTransportConnections: 1,
		ActiveBackendStreams:       1,
		ActivePaths:                1,
		ActiveSessions:             1,
		ConnectionLimitRejections:  1,
		BackendLimitRejections:     1,
		PathReconnects:             1,
		SessionReconnects:          1,
		AuthFailures:               1,
		QueueDrops:                 1,
	}
	if got.ActiveTransportConnections != want.ActiveTransportConnections ||
		got.ActiveBackendStreams != want.ActiveBackendStreams ||
		got.ActivePaths != want.ActivePaths ||
		got.ActiveSessions != want.ActiveSessions ||
		got.ConnectionLimitRejections != want.ConnectionLimitRejections ||
		got.BackendLimitRejections != want.BackendLimitRejections ||
		got.PathReconnects != want.PathReconnects ||
		got.SessionReconnects != want.SessionReconnects ||
		got.AuthFailures != want.AuthFailures ||
		got.QueueDrops != want.QueueDrops {
		t.Fatalf("snapshot = %+v, want %+v", got, want)
	}
	if len(got.Paths) != 2 || got.Paths[0].Active || !got.Paths[1].Active {
		t.Fatalf("path snapshots = %+v, want one closed and one active", got.Paths)
	}
	if got.KCP != (KCPSnapshot{}) {
		t.Fatalf("zero-value registry unexpectedly inherited process KCP counters: %+v", got.KCP)
	}
	path2.Close()
	registry.TransportConnectionClosed()
	registry.BackendStreamClosed()
	closedSnapshot := registry.Snapshot()
	if closedSnapshot.ActiveTransportConnections != 0 || closedSnapshot.ActiveBackendStreams != 0 {
		t.Fatalf("active resources after close = %+v", closedSnapshot)
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
				registry.ConnectionLimitRejected()
				registry.BackendLimitRejected()
			}
		}()
	}
	wg.Wait()

	snapshot := registry.Snapshot()
	want := int64(workers * iterations)
	if snapshot.PathReconnects != want || snapshot.QueueDrops != want ||
		snapshot.ConnectionLimitRejections != want || snapshot.BackendLimitRejections != want {
		t.Fatalf("concurrent snapshot = %+v, want reconnects/drops/rejections %d", snapshot, want)
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
	if snapshot.BytesRead != got.BytesRead || snapshot.BytesWritten != got.BytesWritten ||
		snapshot.ReadOperations != got.ReadOperations || snapshot.WriteOperations != got.WriteOperations ||
		snapshot.ReadErrors != got.ReadErrors || snapshot.WriteErrors != got.WriteErrors ||
		snapshot.WriteLatencySamples != got.WriteLatencySamples ||
		snapshot.WriteLatencyTotalNanoseconds != got.WriteLatencyTotalNanoseconds ||
		snapshot.WriteLatencyMaxNanoseconds != got.WriteLatencyMaxNanoseconds {
		t.Fatalf("process traffic totals = %+v, want retained path totals %+v", snapshot, got)
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
		path := registry.OpenPath("bond/path")
		path.ObserveRead(1, nil)
		path.ObserveWrite(time.Time{}, 2, errors.New("write failed"))
		path.observeWriteLatency(time.Nanosecond)
		path.Close()
	}

	snapshot := registry.Snapshot()
	totalPaths := uint64(recentPathLimit + 10)
	if snapshot.ActivePaths != 0 || len(snapshot.Paths) != recentPathLimit {
		t.Fatalf("snapshot has %d active and %d retained paths, want 0 and %d", snapshot.ActivePaths, len(snapshot.Paths), recentPathLimit)
	}
	if snapshot.Paths[0].ID != 11 || snapshot.Paths[len(snapshot.Paths)-1].ID != recentPathLimit+10 {
		t.Fatalf("retained path IDs = %d..%d, want 11..%d", snapshot.Paths[0].ID, snapshot.Paths[len(snapshot.Paths)-1].ID, recentPathLimit+10)
	}
	if snapshot.BytesRead != totalPaths || snapshot.BytesWritten != totalPaths*2 ||
		snapshot.ReadOperations != totalPaths || snapshot.WriteOperations != totalPaths ||
		snapshot.WriteErrors != totalPaths || snapshot.WriteLatencySamples != totalPaths ||
		snapshot.WriteLatencyTotalNanoseconds != totalPaths || snapshot.WriteLatencyMaxNanoseconds != 1 {
		t.Fatalf("process traffic totals lost evicted paths: %+v", snapshot)
	}
}

func TestSnapshotKCPExportsLibraryCounters(t *testing.T) {
	if Process.kcpSNMP != kcp.DefaultSnmp {
		t.Fatal("process registry is not attached to kcp-go's process-wide collector")
	}
	source := &kcp.Snmp{
		BytesSent:        1,
		BytesReceived:    2,
		MaxConn:          3,
		ActiveOpens:      4,
		PassiveOpens:     5,
		CurrEstab:        6,
		InErrs:           7,
		InCsumErrors:     8,
		KCPInErrors:      9,
		InPkts:           10,
		OutPkts:          11,
		InSegs:           12,
		OutSegs:          13,
		InBytes:          14,
		OutBytes:         15,
		RetransSegs:      16,
		FastRetransSegs:  17,
		EarlyRetransSegs: 18,
		LostSegs:         19,
		RepeatSegs:       20,
		FECRecovered:     21,
		FECErrs:          22,
		FECParityShards:  23,
		FECShortShards:   24,
	}

	registry := Registry{kcpSNMP: source}
	got := registry.Snapshot().KCP
	want := KCPSnapshot{
		ApplicationBytesSent:       1,
		ApplicationBytesReceived:   2,
		MaximumSessions:            3,
		ActiveOpens:                4,
		PassiveOpens:               5,
		CurrentSessions:            6,
		InputErrors:                7,
		ChecksumErrors:             8,
		ProtocolInputErrors:        9,
		PacketsReceived:            10,
		PacketsSent:                11,
		SegmentsReceived:           12,
		SegmentsSent:               13,
		InputBytes:                 14,
		OutputBytes:                15,
		RetransmittedSegments:      16,
		FastRetransmittedSegments:  17,
		EarlyRetransmittedSegments: 18,
		LostSegments:               19,
		RepeatedSegments:           20,
		FECRecoveredPackets:        21,
		FECReportedErrors:          22,
		FECParityShardsReceived:    23,
		FECShortShards:             24,
	}
	if got != want {
		t.Fatalf("KCP snapshot = %+v, want %+v", got, want)
	}
	if got := snapshotKCP(nil); got != (KCPSnapshot{}) {
		t.Fatalf("nil KCP snapshot = %+v, want zero value", got)
	}
}

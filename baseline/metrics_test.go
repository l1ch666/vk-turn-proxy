package baseline

import (
	"testing"

	"github.com/l1ch666/vk-turn-proxy/metrics"
)

func TestDiffMetrics(t *testing.T) {
	before := metrics.Snapshot{
		PathReconnects:               1,
		SessionReconnects:            2,
		AuthFailures:                 3,
		QueueDrops:                   4,
		BytesRead:                    10,
		BytesWritten:                 20,
		ReadOperations:               30,
		WriteOperations:              40,
		ReadErrors:                   5,
		WriteErrors:                  6,
		WriteLatencySamples:          7,
		WriteLatencyTotalNanoseconds: 70,
		KCP: metrics.KCPSnapshot{
			ApplicationBytesSent:    100,
			RetransmittedSegments:   10,
			FECRecoveredPackets:     2,
			FECParityShardsReceived: 8,
		},
	}
	after := before
	after.PathReconnects += 2
	after.SessionReconnects += 3
	after.AuthFailures++
	after.QueueDrops += 4
	after.BytesRead += 1000
	after.BytesWritten += 2000
	after.ReadOperations += 10
	after.WriteOperations += 20
	after.ReadErrors += 2
	after.WriteErrors += 3
	after.WriteLatencySamples += 4
	after.WriteLatencyTotalNanoseconds += 400
	after.KCP.ApplicationBytesSent += 500
	after.KCP.RetransmittedSegments += 5
	after.KCP.FECRecoveredPackets += 3
	after.KCP.FECParityShardsReceived += 7

	delta, err := DiffMetrics(before, after)
	if err != nil {
		t.Fatalf("DiffMetrics: %v", err)
	}
	if delta.PathReconnects != 2 || delta.SessionReconnects != 3 || delta.AuthFailures != 1 || delta.QueueDrops != 4 ||
		delta.BytesRead != 1000 || delta.BytesWritten != 2000 || delta.ReadErrors != 2 || delta.WriteErrors != 3 ||
		delta.WriteLatencySamples != 4 || delta.WriteLatencyTotalNanoseconds != 400 ||
		delta.KCP.ApplicationBytesSent != 500 || delta.KCP.RetransmittedSegments != 5 ||
		delta.KCP.FECRecoveredPackets != 3 || delta.KCP.FECParityShardsReceived != 7 {
		t.Fatalf("metrics delta = %+v", delta)
	}
}

func TestDiffMetricsRejectsCounterReset(t *testing.T) {
	before := metrics.Snapshot{BytesRead: 10}
	after := metrics.Snapshot{BytesRead: 9}
	if _, err := DiffMetrics(before, after); err == nil {
		t.Fatal("counter reset unexpectedly accepted")
	}

	before = metrics.Snapshot{PathReconnects: 2}
	after = metrics.Snapshot{PathReconnects: 1}
	if _, err := DiffMetrics(before, after); err == nil {
		t.Fatal("signed counter reset unexpectedly accepted")
	}
}

func TestDiffMetricsMapsAllKCPCounters(t *testing.T) {
	after := metrics.Snapshot{KCP: metrics.KCPSnapshot{
		ApplicationBytesSent:       1,
		ApplicationBytesReceived:   2,
		ActiveOpens:                3,
		PassiveOpens:               4,
		InputErrors:                5,
		ChecksumErrors:             6,
		ProtocolInputErrors:        7,
		PacketsReceived:            8,
		PacketsSent:                9,
		SegmentsReceived:           10,
		SegmentsSent:               11,
		InputBytes:                 12,
		OutputBytes:                13,
		RetransmittedSegments:      14,
		FastRetransmittedSegments:  15,
		EarlyRetransmittedSegments: 16,
		LostSegments:               17,
		RepeatedSegments:           18,
		FECRecoveredPackets:        19,
		FECReportedErrors:          20,
		FECParityShardsReceived:    21,
		FECShortShards:             22,
	}}
	got, err := DiffMetrics(metrics.Snapshot{}, after)
	if err != nil {
		t.Fatalf("DiffMetrics: %v", err)
	}
	want := KCPCounterDelta{
		ApplicationBytesSent:       1,
		ApplicationBytesReceived:   2,
		ActiveOpens:                3,
		PassiveOpens:               4,
		InputErrors:                5,
		ChecksumErrors:             6,
		ProtocolInputErrors:        7,
		PacketsReceived:            8,
		PacketsSent:                9,
		SegmentsReceived:           10,
		SegmentsSent:               11,
		InputBytes:                 12,
		OutputBytes:                13,
		RetransmittedSegments:      14,
		FastRetransmittedSegments:  15,
		EarlyRetransmittedSegments: 16,
		LostSegments:               17,
		RepeatedSegments:           18,
		FECRecoveredPackets:        19,
		FECReportedErrors:          20,
		FECParityShardsReceived:    21,
		FECShortShards:             22,
	}
	if got.KCP != want {
		t.Fatalf("KCP delta = %+v, want %+v", got.KCP, want)
	}
}

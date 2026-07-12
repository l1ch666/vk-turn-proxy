package baseline

import (
	"fmt"

	"github.com/l1ch666/vk-turn-proxy/metrics"
)

type MetricDelta struct {
	PathReconnects               int64           `json:"path_reconnects"`
	SessionReconnects            int64           `json:"session_reconnects"`
	AuthFailures                 int64           `json:"auth_failures"`
	QueueDrops                   int64           `json:"queue_drops"`
	BytesRead                    uint64          `json:"bytes_read"`
	BytesWritten                 uint64          `json:"bytes_written"`
	ReadOperations               uint64          `json:"read_operations"`
	WriteOperations              uint64          `json:"write_operations"`
	ReadErrors                   uint64          `json:"read_errors"`
	WriteErrors                  uint64          `json:"write_errors"`
	WriteLatencySamples          uint64          `json:"write_latency_samples"`
	WriteLatencyTotalNanoseconds uint64          `json:"write_latency_total_nanoseconds"`
	KCP                          KCPCounterDelta `json:"kcp"`
}

type KCPCounterDelta struct {
	ApplicationBytesSent       uint64 `json:"application_bytes_sent"`
	ApplicationBytesReceived   uint64 `json:"application_bytes_received"`
	ActiveOpens                uint64 `json:"active_opens"`
	PassiveOpens               uint64 `json:"passive_opens"`
	InputErrors                uint64 `json:"input_errors"`
	ChecksumErrors             uint64 `json:"checksum_errors"`
	ProtocolInputErrors        uint64 `json:"protocol_input_errors"`
	PacketsReceived            uint64 `json:"packets_received"`
	PacketsSent                uint64 `json:"packets_sent"`
	SegmentsReceived           uint64 `json:"segments_received"`
	SegmentsSent               uint64 `json:"segments_sent"`
	InputBytes                 uint64 `json:"input_bytes"`
	OutputBytes                uint64 `json:"output_bytes"`
	RetransmittedSegments      uint64 `json:"retransmitted_segments"`
	FastRetransmittedSegments  uint64 `json:"fast_retransmitted_segments"`
	EarlyRetransmittedSegments uint64 `json:"early_retransmitted_segments"`
	LostSegments               uint64 `json:"lost_segments"`
	RepeatedSegments           uint64 `json:"repeated_segments"`
	FECRecoveredPackets        uint64 `json:"fec_recovered_packets"`
	FECReportedErrors          uint64 `json:"fec_reported_errors"`
	FECParityShardsReceived    uint64 `json:"fec_parity_shards_received"`
	FECShortShards             uint64 `json:"fec_short_shards"`
}

func DiffMetrics(before, after metrics.Snapshot) (MetricDelta, error) {
	var delta MetricDelta
	intCounters := []struct {
		name          string
		before, after int64
		target        *int64
	}{
		{"path_reconnects", before.PathReconnects, after.PathReconnects, &delta.PathReconnects},
		{"session_reconnects", before.SessionReconnects, after.SessionReconnects, &delta.SessionReconnects},
		{"auth_failures", before.AuthFailures, after.AuthFailures, &delta.AuthFailures},
		{"queue_drops", before.QueueDrops, after.QueueDrops, &delta.QueueDrops},
	}
	for _, counter := range intCounters {
		if counter.before < 0 || counter.after < counter.before {
			return MetricDelta{}, fmt.Errorf("metrics counter %s decreased from %d to %d", counter.name, counter.before, counter.after)
		}
		*counter.target = counter.after - counter.before
	}

	uintCounters := []struct {
		name          string
		before, after uint64
		target        *uint64
	}{
		{"bytes_read", before.BytesRead, after.BytesRead, &delta.BytesRead},
		{"bytes_written", before.BytesWritten, after.BytesWritten, &delta.BytesWritten},
		{"read_operations", before.ReadOperations, after.ReadOperations, &delta.ReadOperations},
		{"write_operations", before.WriteOperations, after.WriteOperations, &delta.WriteOperations},
		{"read_errors", before.ReadErrors, after.ReadErrors, &delta.ReadErrors},
		{"write_errors", before.WriteErrors, after.WriteErrors, &delta.WriteErrors},
		{"write_latency_samples", before.WriteLatencySamples, after.WriteLatencySamples, &delta.WriteLatencySamples},
		{"write_latency_total_nanoseconds", before.WriteLatencyTotalNanoseconds, after.WriteLatencyTotalNanoseconds, &delta.WriteLatencyTotalNanoseconds},
		{"kcp.application_bytes_sent", before.KCP.ApplicationBytesSent, after.KCP.ApplicationBytesSent, &delta.KCP.ApplicationBytesSent},
		{"kcp.application_bytes_received", before.KCP.ApplicationBytesReceived, after.KCP.ApplicationBytesReceived, &delta.KCP.ApplicationBytesReceived},
		{"kcp.active_opens", before.KCP.ActiveOpens, after.KCP.ActiveOpens, &delta.KCP.ActiveOpens},
		{"kcp.passive_opens", before.KCP.PassiveOpens, after.KCP.PassiveOpens, &delta.KCP.PassiveOpens},
		{"kcp.input_errors", before.KCP.InputErrors, after.KCP.InputErrors, &delta.KCP.InputErrors},
		{"kcp.checksum_errors", before.KCP.ChecksumErrors, after.KCP.ChecksumErrors, &delta.KCP.ChecksumErrors},
		{"kcp.protocol_input_errors", before.KCP.ProtocolInputErrors, after.KCP.ProtocolInputErrors, &delta.KCP.ProtocolInputErrors},
		{"kcp.packets_received", before.KCP.PacketsReceived, after.KCP.PacketsReceived, &delta.KCP.PacketsReceived},
		{"kcp.packets_sent", before.KCP.PacketsSent, after.KCP.PacketsSent, &delta.KCP.PacketsSent},
		{"kcp.segments_received", before.KCP.SegmentsReceived, after.KCP.SegmentsReceived, &delta.KCP.SegmentsReceived},
		{"kcp.segments_sent", before.KCP.SegmentsSent, after.KCP.SegmentsSent, &delta.KCP.SegmentsSent},
		{"kcp.input_bytes", before.KCP.InputBytes, after.KCP.InputBytes, &delta.KCP.InputBytes},
		{"kcp.output_bytes", before.KCP.OutputBytes, after.KCP.OutputBytes, &delta.KCP.OutputBytes},
		{"kcp.retransmitted_segments", before.KCP.RetransmittedSegments, after.KCP.RetransmittedSegments, &delta.KCP.RetransmittedSegments},
		{"kcp.fast_retransmitted_segments", before.KCP.FastRetransmittedSegments, after.KCP.FastRetransmittedSegments, &delta.KCP.FastRetransmittedSegments},
		{"kcp.early_retransmitted_segments", before.KCP.EarlyRetransmittedSegments, after.KCP.EarlyRetransmittedSegments, &delta.KCP.EarlyRetransmittedSegments},
		{"kcp.lost_segments", before.KCP.LostSegments, after.KCP.LostSegments, &delta.KCP.LostSegments},
		{"kcp.repeated_segments", before.KCP.RepeatedSegments, after.KCP.RepeatedSegments, &delta.KCP.RepeatedSegments},
		{"kcp.fec_recovered_packets", before.KCP.FECRecoveredPackets, after.KCP.FECRecoveredPackets, &delta.KCP.FECRecoveredPackets},
		{"kcp.fec_reported_errors", before.KCP.FECReportedErrors, after.KCP.FECReportedErrors, &delta.KCP.FECReportedErrors},
		{"kcp.fec_parity_shards_received", before.KCP.FECParityShardsReceived, after.KCP.FECParityShardsReceived, &delta.KCP.FECParityShardsReceived},
		{"kcp.fec_short_shards", before.KCP.FECShortShards, after.KCP.FECShortShards, &delta.KCP.FECShortShards},
	}
	for _, counter := range uintCounters {
		if counter.after < counter.before {
			return MetricDelta{}, fmt.Errorf("metrics counter %s decreased from %d to %d", counter.name, counter.before, counter.after)
		}
		*counter.target = counter.after - counter.before
	}
	return delta, nil
}

package metrics

import (
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/xtaci/kcp-go/v5"
)

const (
	recentPathLimit        = 128
	writeLatencySampleRate = 64
)

// PathSnapshot is the traffic observed on one physical bonded transport path.
// Closed paths remain visible in a bounded recent-history window so the error
// that caused a reconnect is not lost as soon as the replacement path opens.
type PathSnapshot struct {
	ID                           uint64 `json:"id"`
	Label                        string `json:"label"`
	Active                       bool   `json:"active"`
	BytesRead                    uint64 `json:"bytes_read"`
	BytesWritten                 uint64 `json:"bytes_written"`
	ReadOperations               uint64 `json:"read_operations"`
	WriteOperations              uint64 `json:"write_operations"`
	ReadErrors                   uint64 `json:"read_errors"`
	WriteErrors                  uint64 `json:"write_errors"`
	WriteLatencySamples          uint64 `json:"write_latency_samples"`
	WriteLatencyTotalNanoseconds uint64 `json:"write_latency_total_nanoseconds"`
	WriteLatencyMaxNanoseconds   uint64 `json:"write_latency_max_nanoseconds"`
}

// KCPSnapshot contains the process-wide counters exported by kcp-go. The
// dependency does not expose retransmission or FEC counters per session, so
// these values aggregate every KCP session in this process.
type KCPSnapshot struct {
	ApplicationBytesSent       uint64 `json:"application_bytes_sent"`
	ApplicationBytesReceived   uint64 `json:"application_bytes_received"`
	CurrentSessions            uint64 `json:"current_sessions"`
	MaximumSessions            uint64 `json:"maximum_sessions"`
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

// Snapshot is a consistent-enough view of process transport metrics.
// Individual atomic fields may advance while a snapshot is being read;
// counters remain monotonic and gauges reflect a nearby point in time.
type Snapshot struct {
	ActiveTransportConnections   int64          `json:"active_transport_connections"`
	ActivePaths                  int64          `json:"active_paths"`
	ActiveSessions               int64          `json:"active_sessions"`
	ConnectionLimitRejections    int64          `json:"connection_limit_rejections"`
	PathReconnects               int64          `json:"path_reconnects"`
	SessionReconnects            int64          `json:"session_reconnects"`
	AuthFailures                 int64          `json:"auth_failures"`
	QueueDrops                   int64          `json:"queue_drops"`
	BytesRead                    uint64         `json:"bytes_read"`
	BytesWritten                 uint64         `json:"bytes_written"`
	ReadOperations               uint64         `json:"read_operations"`
	WriteOperations              uint64         `json:"write_operations"`
	ReadErrors                   uint64         `json:"read_errors"`
	WriteErrors                  uint64         `json:"write_errors"`
	WriteLatencySamples          uint64         `json:"write_latency_samples"`
	WriteLatencyTotalNanoseconds uint64         `json:"write_latency_total_nanoseconds"`
	WriteLatencyMaxNanoseconds   uint64         `json:"write_latency_max_nanoseconds"`
	KCP                          KCPSnapshot    `json:"kcp"`
	Paths                        []PathSnapshot `json:"paths"`
}

// Registry stores process counters and a bounded set of per-path statistics.
// Its zero value is ready to use.
type Registry struct {
	activeTransportConnections atomic.Int64
	activePaths                atomic.Int64
	activeSessions             atomic.Int64
	connectionLimitRejections  atomic.Int64
	pathReconnects             atomic.Int64
	sessionReconnects          atomic.Int64
	authFailures               atomic.Int64
	queueDrops                 atomic.Int64
	nextPathID                 atomic.Uint64
	bytesRead                  atomic.Uint64
	bytesWritten               atomic.Uint64
	readOperations             atomic.Uint64
	writeOperations            atomic.Uint64
	readErrors                 atomic.Uint64
	writeErrors                atomic.Uint64
	writeLatencySamples        atomic.Uint64
	writeLatencyTotalNS        atomic.Uint64
	writeLatencyMaxNS          atomic.Uint64
	kcpSNMP                    *kcp.Snmp

	pathsMu         sync.RWMutex
	activePathsByID map[uint64]*Path
	recentPaths     []*Path
}

// Path holds the lock-free counters for one physical bonded connection.
type Path struct {
	registry *Registry
	id       uint64
	label    string
	closed   atomic.Bool

	bytesRead           atomic.Uint64
	bytesWritten        atomic.Uint64
	readOperations      atomic.Uint64
	writeOperations     atomic.Uint64
	readErrors          atomic.Uint64
	writeErrors         atomic.Uint64
	writeSampleSequence atomic.Uint64
	writeLatencySamples atomic.Uint64
	writeLatencyTotalNS atomic.Uint64
	writeLatencyMaxNS   atomic.Uint64
}

// Process is the registry used by the client and server binaries. KCP's
// process-wide SNMP collector is attached only here so zero-value registries
// remain isolated in tests and other callers.
var Process = Registry{kcpSNMP: kcp.DefaultSnmp}

// OpenPath starts accounting for a physical bonded transport path.
func (r *Registry) OpenPath(label string) *Path {
	path := &Path{
		registry: r,
		id:       r.nextPathID.Add(1),
		label:    label,
	}

	r.pathsMu.Lock()
	if r.activePathsByID == nil {
		r.activePathsByID = make(map[uint64]*Path)
	}
	r.activePathsByID[path.id] = path
	r.activePaths.Add(1)
	r.pathsMu.Unlock()
	return path
}

func (r *Registry) TransportConnectionOpened() { r.activeTransportConnections.Add(1) }
func (r *Registry) TransportConnectionClosed() { r.activeTransportConnections.Add(-1) }
func (r *Registry) ConnectionLimitRejected()   { r.connectionLimitRejections.Add(1) }
func (r *Registry) SessionOpened()             { r.activeSessions.Add(1) }
func (r *Registry) SessionClosed()             { r.activeSessions.Add(-1) }
func (r *Registry) PathReconnected()           { r.pathReconnects.Add(1) }
func (r *Registry) SessionReconnected()        { r.sessionReconnects.Add(1) }
func (r *Registry) AuthFailed()                { r.authFailures.Add(1) }
func (r *Registry) QueueDropped()              { r.queueDrops.Add(1) }

// Close stops accounting for a path as active. It is safe to call repeatedly.
func (p *Path) Close() {
	if p == nil || p.registry == nil {
		return
	}
	r := p.registry
	r.pathsMu.Lock()
	if !p.closed.CompareAndSwap(false, true) {
		r.pathsMu.Unlock()
		return
	}
	if _, ok := r.activePathsByID[p.id]; ok {
		delete(r.activePathsByID, p.id)
		r.activePaths.Add(-1)
		if len(r.recentPaths) < recentPathLimit {
			r.recentPaths = append(r.recentPaths, p)
		} else {
			copy(r.recentPaths, r.recentPaths[1:])
			r.recentPaths[len(r.recentPaths)-1] = p
		}
	}
	r.pathsMu.Unlock()
}

// ObserveRead records one read attempt and its returned byte count.
func (p *Path) ObserveRead(n int, err error) {
	if p == nil {
		return
	}
	p.readOperations.Add(1)
	p.registry.readOperations.Add(1)
	if n > 0 {
		p.bytesRead.Add(uint64(n))
		p.registry.bytesRead.Add(uint64(n))
	}
	if err != nil {
		p.readErrors.Add(1)
		p.registry.readErrors.Add(1)
	}
}

// BeginWrite returns a sampling timestamp for approximately one in every 64
// writes. A zero timestamp intentionally avoids time.Now on unsampled writes.
func (p *Path) BeginWrite() time.Time {
	if p == nil || (p.writeSampleSequence.Add(1)-1)%writeLatencySampleRate != 0 {
		return time.Time{}
	}
	return time.Now()
}

// ObserveWrite records one write attempt and, when BeginWrite sampled it, its
// elapsed duration.
func (p *Path) ObserveWrite(started time.Time, n int, err error) {
	if p == nil {
		return
	}
	p.writeOperations.Add(1)
	p.registry.writeOperations.Add(1)
	if n > 0 {
		p.bytesWritten.Add(uint64(n))
		p.registry.bytesWritten.Add(uint64(n))
	}
	if err != nil {
		p.writeErrors.Add(1)
		p.registry.writeErrors.Add(1)
	}
	if !started.IsZero() {
		p.observeWriteLatency(time.Since(started))
	}
}

func (p *Path) observeWriteLatency(elapsed time.Duration) {
	if elapsed < 0 {
		elapsed = 0
	}
	ns := uint64(elapsed)
	p.writeLatencySamples.Add(1)
	p.writeLatencyTotalNS.Add(ns)
	p.registry.writeLatencySamples.Add(1)
	p.registry.writeLatencyTotalNS.Add(ns)
	updateMaximum(&p.writeLatencyMaxNS, ns)
	updateMaximum(&p.registry.writeLatencyMaxNS, ns)
}

func updateMaximum(counter *atomic.Uint64, value uint64) {
	for current := counter.Load(); value > current; current = counter.Load() {
		if counter.CompareAndSwap(current, value) {
			break
		}
	}
}

func (p *Path) snapshot() PathSnapshot {
	return PathSnapshot{
		ID:                           p.id,
		Label:                        p.label,
		Active:                       !p.closed.Load(),
		BytesRead:                    p.bytesRead.Load(),
		BytesWritten:                 p.bytesWritten.Load(),
		ReadOperations:               p.readOperations.Load(),
		WriteOperations:              p.writeOperations.Load(),
		ReadErrors:                   p.readErrors.Load(),
		WriteErrors:                  p.writeErrors.Load(),
		WriteLatencySamples:          p.writeLatencySamples.Load(),
		WriteLatencyTotalNanoseconds: p.writeLatencyTotalNS.Load(),
		WriteLatencyMaxNanoseconds:   p.writeLatencyMaxNS.Load(),
	}
}

func (r *Registry) pathSnapshots() (int64, []PathSnapshot) {
	r.pathsMu.RLock()
	active := r.activePaths.Load()
	paths := make([]PathSnapshot, 0, len(r.activePathsByID)+len(r.recentPaths))
	for _, path := range r.activePathsByID {
		paths = append(paths, path.snapshot())
	}
	for _, path := range r.recentPaths {
		paths = append(paths, path.snapshot())
	}
	r.pathsMu.RUnlock()
	sort.Slice(paths, func(i, j int) bool { return paths[i].ID < paths[j].ID })
	return active, paths
}

func (r *Registry) Snapshot() Snapshot {
	activePaths, paths := r.pathSnapshots()
	return Snapshot{
		ActiveTransportConnections:   r.activeTransportConnections.Load(),
		ActivePaths:                  activePaths,
		ActiveSessions:               r.activeSessions.Load(),
		ConnectionLimitRejections:    r.connectionLimitRejections.Load(),
		PathReconnects:               r.pathReconnects.Load(),
		SessionReconnects:            r.sessionReconnects.Load(),
		AuthFailures:                 r.authFailures.Load(),
		QueueDrops:                   r.queueDrops.Load(),
		BytesRead:                    r.bytesRead.Load(),
		BytesWritten:                 r.bytesWritten.Load(),
		ReadOperations:               r.readOperations.Load(),
		WriteOperations:              r.writeOperations.Load(),
		ReadErrors:                   r.readErrors.Load(),
		WriteErrors:                  r.writeErrors.Load(),
		WriteLatencySamples:          r.writeLatencySamples.Load(),
		WriteLatencyTotalNanoseconds: r.writeLatencyTotalNS.Load(),
		WriteLatencyMaxNanoseconds:   r.writeLatencyMaxNS.Load(),
		KCP:                          snapshotKCP(r.kcpSNMP),
		Paths:                        paths,
	}
}

func snapshotKCP(source *kcp.Snmp) KCPSnapshot {
	if source == nil {
		return KCPSnapshot{}
	}
	stats := source.Copy()
	return KCPSnapshot{
		ApplicationBytesSent:       stats.BytesSent,
		ApplicationBytesReceived:   stats.BytesReceived,
		CurrentSessions:            stats.CurrEstab,
		MaximumSessions:            stats.MaxConn,
		ActiveOpens:                stats.ActiveOpens,
		PassiveOpens:               stats.PassiveOpens,
		InputErrors:                stats.InErrs,
		ChecksumErrors:             stats.InCsumErrors,
		ProtocolInputErrors:        stats.KCPInErrors,
		PacketsReceived:            stats.InPkts,
		PacketsSent:                stats.OutPkts,
		SegmentsReceived:           stats.InSegs,
		SegmentsSent:               stats.OutSegs,
		InputBytes:                 stats.InBytes,
		OutputBytes:                stats.OutBytes,
		RetransmittedSegments:      stats.RetransSegs,
		FastRetransmittedSegments:  stats.FastRetransSegs,
		EarlyRetransmittedSegments: stats.EarlyRetransSegs,
		LostSegments:               stats.LostSegs,
		RepeatedSegments:           stats.RepeatSegs,
		FECRecoveredPackets:        stats.FECRecovered,
		FECReportedErrors:          stats.FECErrs,
		FECParityShardsReceived:    stats.FECParityShards,
		FECShortShards:             stats.FECShortShards,
	}
}

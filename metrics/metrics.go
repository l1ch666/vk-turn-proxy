package metrics

import "sync/atomic"

// Snapshot is a consistent-enough lock-free view of process transport metrics.
// Individual fields may advance while a snapshot is being read; counters remain
// monotonic and gauges reflect a nearby point in time.
type Snapshot struct {
	ActivePaths       int64 `json:"active_paths"`
	ActiveSessions    int64 `json:"active_sessions"`
	PathReconnects    int64 `json:"path_reconnects"`
	SessionReconnects int64 `json:"session_reconnects"`
	AuthFailures      int64 `json:"auth_failures"`
	QueueDrops        int64 `json:"queue_drops"`
}

// Registry stores lock-free process counters. Its zero value is ready to use.
type Registry struct {
	activePaths       atomic.Int64
	activeSessions    atomic.Int64
	pathReconnects    atomic.Int64
	sessionReconnects atomic.Int64
	authFailures      atomic.Int64
	queueDrops        atomic.Int64
}

// Process is the registry used by the client and server binaries.
var Process Registry

func (r *Registry) PathOpened()         { r.activePaths.Add(1) }
func (r *Registry) PathClosed()         { r.activePaths.Add(-1) }
func (r *Registry) SessionOpened()      { r.activeSessions.Add(1) }
func (r *Registry) SessionClosed()      { r.activeSessions.Add(-1) }
func (r *Registry) PathReconnected()    { r.pathReconnects.Add(1) }
func (r *Registry) SessionReconnected() { r.sessionReconnects.Add(1) }
func (r *Registry) AuthFailed()         { r.authFailures.Add(1) }
func (r *Registry) QueueDropped()       { r.queueDrops.Add(1) }

func (r *Registry) Snapshot() Snapshot {
	return Snapshot{
		ActivePaths:       r.activePaths.Load(),
		ActiveSessions:    r.activeSessions.Load(),
		PathReconnects:    r.pathReconnects.Load(),
		SessionReconnects: r.sessionReconnects.Load(),
		AuthFailures:      r.authFailures.Load(),
		QueueDrops:        r.queueDrops.Load(),
	}
}

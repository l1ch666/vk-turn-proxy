package main

import (
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/l1ch666/vk-turn-proxy/metrics"
)

const (
	defaultMaxBackendConnections  = 1024
	defaultMaxStreamsPerSession   = 256
	maximumConfiguredBackendLimit = 65535
)

type backendRejection string

const (
	backendAccepted           backendRejection = ""
	backendRejectedGlobal     backendRejection = "global backend connection limit reached"
	backendRejectedPerSession backendRejection = "per-session stream limit reached"
)

type concurrencyLimiter struct {
	maximum int64
	active  atomic.Int64
}

type concurrencyLease struct {
	limiter *concurrencyLimiter
	once    sync.Once
}

type backendGate struct {
	global               *concurrencyLimiter
	maxStreamsPerSession int
	registry             *metrics.Registry
	rejections           atomic.Uint64
}

type backendLease struct {
	global   *concurrencyLease
	session  *concurrencyLease
	registry *metrics.Registry
	once     sync.Once
}

func newConcurrencyLimiter(maximum int) (*concurrencyLimiter, error) {
	if maximum < 1 || maximum > maximumConfiguredBackendLimit {
		return nil, fmt.Errorf("concurrency limit must be in 1..%d", maximumConfiguredBackendLimit)
	}
	return &concurrencyLimiter{maximum: int64(maximum)}, nil
}

func newBackendGate(maximumGlobal, maximumPerSession int, registry *metrics.Registry) (*backendGate, error) {
	global, err := newConcurrencyLimiter(maximumGlobal)
	if err != nil {
		return nil, fmt.Errorf("maximum backend connections: %w", err)
	}
	if maximumPerSession < 1 || maximumPerSession > maximumGlobal {
		return nil, fmt.Errorf("maximum streams per session must be in 1..%d", maximumGlobal)
	}
	if registry == nil {
		return nil, fmt.Errorf("backend metrics registry must not be nil")
	}
	return &backendGate{
		global:               global,
		maxStreamsPerSession: maximumPerSession,
		registry:             registry,
	}, nil
}

func defaultBackendGate() *backendGate {
	gate, err := newBackendGate(defaultMaxBackendConnections, defaultMaxStreamsPerSession, &metrics.Process)
	if err != nil {
		panic(err)
	}
	return gate
}

func (l *concurrencyLimiter) Acquire() *concurrencyLease {
	if l == nil {
		return nil
	}
	for {
		current := l.active.Load()
		if current >= l.maximum {
			return nil
		}
		if l.active.CompareAndSwap(current, current+1) {
			return &concurrencyLease{limiter: l}
		}
	}
}

func (l *concurrencyLease) Release() bool {
	if l == nil || l.limiter == nil {
		return false
	}
	released := false
	l.once.Do(func() {
		l.limiter.active.Add(-1)
		released = true
	})
	return released
}

func (g *backendGate) NewSessionLimiter() *concurrencyLimiter {
	limiter, err := newConcurrencyLimiter(g.maxStreamsPerSession)
	if err != nil {
		panic(err)
	}
	return limiter
}

func (g *backendGate) Acquire(session *concurrencyLimiter) (*backendLease, backendRejection, uint64) {
	if g == nil || g.global == nil || g.registry == nil || session == nil {
		return nil, backendRejectedGlobal, g.recordRejection()
	}
	sessionLease := session.Acquire()
	if sessionLease == nil {
		return nil, backendRejectedPerSession, g.recordRejection()
	}
	globalLease := g.global.Acquire()
	if globalLease == nil {
		sessionLease.Release()
		return nil, backendRejectedGlobal, g.recordRejection()
	}
	g.registry.BackendStreamOpened()
	return &backendLease{global: globalLease, session: sessionLease, registry: g.registry}, backendAccepted, 0
}

func (g *backendGate) recordRejection() uint64 {
	if g == nil {
		return 1
	}
	if g.registry != nil {
		g.registry.BackendLimitRejected()
	}
	return g.rejections.Add(1)
}

func (l *backendLease) Release() bool {
	if l == nil {
		return false
	}
	released := false
	l.once.Do(func() {
		l.session.Release()
		l.global.Release()
		l.registry.BackendStreamClosed()
		released = true
	})
	return released
}

func (l *concurrencyLimiter) Active() int64 {
	if l == nil {
		return 0
	}
	return l.active.Load()
}

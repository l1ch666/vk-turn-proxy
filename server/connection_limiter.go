package main

import (
	"fmt"
	"net"
	"sync"
)

const (
	defaultMaxTransportConnections      = 256
	defaultMaxTransportConnectionsPerIP = 64
	maximumConfiguredConnectionLimit    = 65535
)

type connectionRejection string

const (
	connectionAccepted          connectionRejection = ""
	connectionRejectedInvalidIP connectionRejection = "remote address has no valid IP"
	connectionRejectedGlobal    connectionRejection = "global transport connection limit reached"
	connectionRejectedPerIP     connectionRejection = "per-IP transport connection limit reached"
)

type connectionLimiter struct {
	mu       sync.Mutex
	maxTotal int
	maxPerIP int
	total    int
	perIP    map[string]int
}

type connectionLease struct {
	limiter *connectionLimiter
	ip      string
	once    sync.Once
}

func newConnectionLimiter(maxTotal, maxPerIP int) (*connectionLimiter, error) {
	if maxTotal < 1 || maxTotal > maximumConfiguredConnectionLimit {
		return nil, fmt.Errorf("maximum transport connections must be in 1..%d", maximumConfiguredConnectionLimit)
	}
	if maxPerIP < 1 || maxPerIP > maxTotal {
		return nil, fmt.Errorf("maximum transport connections per IP must be in 1..%d", maxTotal)
	}
	return &connectionLimiter{
		maxTotal: maxTotal,
		maxPerIP: maxPerIP,
		perIP:    make(map[string]int),
	}, nil
}

func (l *connectionLimiter) Acquire(remote net.Addr) (*connectionLease, connectionRejection) {
	ip, err := remoteIPKey(remote)
	if err != nil {
		return nil, connectionRejectedInvalidIP
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	if l.total >= l.maxTotal {
		return nil, connectionRejectedGlobal
	}
	if l.perIP[ip] >= l.maxPerIP {
		return nil, connectionRejectedPerIP
	}
	l.total++
	l.perIP[ip]++
	return &connectionLease{limiter: l, ip: ip}, connectionAccepted
}

// Release returns true only for the call that actually returned the slot.
// It is safe to invoke from competing connection and bond cleanup paths.
func (l *connectionLease) Release() bool {
	if l == nil || l.limiter == nil {
		return false
	}
	released := false
	l.once.Do(func() {
		l.limiter.release(l.ip)
		released = true
	})
	return released
}

func (l *connectionLimiter) release(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if count := l.perIP[ip]; count <= 1 {
		delete(l.perIP, ip)
	} else {
		l.perIP[ip] = count - 1
	}
	if l.total > 0 {
		l.total--
	}
}

func (l *connectionLimiter) counts(ip string) (total, perIP int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.total, l.perIP[ip]
}

func remoteIPKey(remote net.Addr) (string, error) {
	if remote == nil {
		return "", fmt.Errorf("remote address is nil")
	}
	var ip net.IP
	switch address := remote.(type) {
	case *net.UDPAddr:
		ip = address.IP
	case *net.TCPAddr:
		ip = address.IP
	default:
		host, _, err := net.SplitHostPort(remote.String())
		if err != nil {
			return "", fmt.Errorf("split remote address: %w", err)
		}
		ip = net.ParseIP(host)
	}
	if ipv4 := ip.To4(); ipv4 != nil {
		return ipv4.String(), nil
	}
	if ipv6 := ip.To16(); ipv6 != nil {
		return ipv6.String(), nil
	}
	return "", fmt.Errorf("remote address has no valid IP")
}

func shouldLogResourceRejection(count uint64) bool {
	return count != 0 && count&(count-1) == 0
}

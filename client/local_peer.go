package main

import (
	"net"
	"sync"
	"time"
)

const defaultLocalPeerRebindIdle = time.Minute

// localPeerPin keeps return traffic bound to one local UDP peer. A different
// sender cannot steal the flow while the current peer is active, but a client
// restart with a new source port can recover after a bounded idle period.
type localPeerPin struct {
	mu          sync.RWMutex
	addr        net.Addr
	lastSeen    time.Time
	rebindAfter time.Duration
}

func newLocalPeerPin(rebindAfter time.Duration) *localPeerPin {
	if rebindAfter <= 0 {
		rebindAfter = defaultLocalPeerRebindIdle
	}
	return &localPeerPin{rebindAfter: rebindAfter}
}

func (pin *localPeerPin) Accept(addr net.Addr, now time.Time) bool {
	if addr == nil {
		return false
	}
	pin.mu.Lock()
	defer pin.mu.Unlock()
	if pin.addr == nil {
		pin.addr = cloneNetAddr(addr)
		pin.lastSeen = now
		return true
	}
	if pin.addr.Network() == addr.Network() && pin.addr.String() == addr.String() {
		pin.lastSeen = now
		return true
	}
	if now.Sub(pin.lastSeen) >= pin.rebindAfter {
		pin.addr = cloneNetAddr(addr)
		pin.lastSeen = now
		return true
	}
	return false
}

func (pin *localPeerPin) Current() net.Addr {
	pin.mu.RLock()
	defer pin.mu.RUnlock()
	return pin.addr
}

func cloneNetAddr(addr net.Addr) net.Addr {
	switch typed := addr.(type) {
	case *net.UDPAddr:
		clone := *typed
		clone.IP = append(net.IP(nil), typed.IP...)
		return &clone
	case *net.TCPAddr:
		clone := *typed
		clone.IP = append(net.IP(nil), typed.IP...)
		return &clone
	default:
		return addr
	}
}

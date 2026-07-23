package main

import (
	"net"
	"testing"
	"time"
)

func TestLocalPeerPinRejectsActiveTakeoverAndAllowsIdleRebind(t *testing.T) {
	pin := newLocalPeerPin(time.Minute)
	started := time.Unix(1000, 0)
	first := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 10001}
	second := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 10002}

	if !pin.Accept(first, started) {
		t.Fatal("first local peer was rejected")
	}
	if pin.Accept(second, started.Add(59*time.Second)) {
		t.Fatal("different local peer stole an active pin")
	}
	if got := pin.Current(); got == nil || got.String() != first.String() {
		t.Fatalf("current peer = %v, want %v", got, first)
	}
	if !pin.Accept(second, started.Add(time.Minute)) {
		t.Fatal("new local peer was not accepted after idle rebind interval")
	}
	if got := pin.Current(); got == nil || got.String() != second.String() {
		t.Fatalf("current peer after rebind = %v, want %v", got, second)
	}
}

func TestLocalPeerPinSamePeerRefreshesLease(t *testing.T) {
	pin := newLocalPeerPin(time.Minute)
	started := time.Unix(2000, 0)
	first := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 10001}
	second := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 10002}

	if !pin.Accept(first, started) || !pin.Accept(first, started.Add(50*time.Second)) {
		t.Fatal("same peer did not refresh its pin")
	}
	if pin.Accept(second, started.Add(70*time.Second)) {
		t.Fatal("peer rebind ignored the refreshed activity timestamp")
	}
	if !pin.Accept(second, started.Add(111*time.Second)) {
		t.Fatal("peer did not rebind after refreshed lease became idle")
	}
}

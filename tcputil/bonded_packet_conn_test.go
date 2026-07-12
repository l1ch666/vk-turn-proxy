package tcputil

import (
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cacggghp/vk-turn-proxy/metrics"
)

func TestBondedPacketConnWritesAcrossPaths(t *testing.T) {
	t.Parallel()

	pc := NewBondedPacketConn("test")
	defer func() { _ = pc.Close() }()

	left1, right1 := net.Pipe()
	defer func() { _ = right1.Close() }()
	left2, right2 := net.Pipe()
	defer func() { _ = right2.Close() }()

	pc.AddConn(left1, nil)
	pc.AddConn(left2, nil)

	received := make(chan string, 2)
	readOne := func(label string, c net.Conn) {
		buf := make([]byte, 8)
		_ = c.SetReadDeadline(time.Now().Add(time.Second))
		n, err := c.Read(buf)
		if err != nil {
			received <- label + ":ERR"
			return
		}
		received <- label + ":" + string(buf[:n])
	}
	go readOne("p1", right1)
	go readOne("p2", right2)

	if _, err := pc.WriteTo([]byte("a"), nil); err != nil {
		t.Fatalf("first WriteTo failed: %v", err)
	}
	if _, err := pc.WriteTo([]byte("b"), nil); err != nil {
		t.Fatalf("second WriteTo failed: %v", err)
	}

	got := map[string]bool{<-received: true, <-received: true}
	if !got["p1:a"] || !got["p2:b"] {
		t.Fatalf("writes were not distributed in path order: %v", got)
	}
}

func TestBondedPacketConnReadsFromAnyPath(t *testing.T) {
	t.Parallel()

	pc := NewBondedPacketConn("test")
	defer func() { _ = pc.Close() }()

	left, right := net.Pipe()
	defer func() { _ = right.Close() }()
	pc.AddConn(left, nil)

	go func() {
		_, _ = right.Write([]byte("packet"))
	}()

	buf := make([]byte, 32)
	n, _, err := pc.ReadFrom(buf)
	if err != nil {
		t.Fatalf("ReadFrom failed: %v", err)
	}
	if string(buf[:n]) != "packet" {
		t.Fatalf("ReadFrom = %q, want packet", string(buf[:n]))
	}
}

func TestBondedPacketConnNotifiesPathStateChanges(t *testing.T) {
	pc := NewBondedPacketConn("test-state")
	defer func() { _ = pc.Close() }()

	left, right := net.Pipe()
	done := pc.AddConn(left, nil)
	select {
	case <-pc.StateChanged():
	case <-time.After(time.Second):
		t.Fatal("no notification after adding a path")
	}
	if pc.Count() != 1 {
		t.Fatalf("path count after add = %d, want 1", pc.Count())
	}

	if err := right.Close(); err != nil {
		t.Fatalf("close peer path: %v", err)
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("path reader did not stop")
	}
	select {
	case <-pc.StateChanged():
	case <-time.After(time.Second):
		t.Fatal("no notification after removing a path")
	}
	if pc.Count() != 0 {
		t.Fatalf("path count after removal = %d, want 0", pc.Count())
	}
}

func TestBondedPacketConnRecordsPathTraffic(t *testing.T) {
	var registry metrics.Registry
	pc := NewBondedPacketConn("test-metrics")
	pc.metricRegistry = &registry
	defer func() { _ = pc.Close() }()

	left, right := net.Pipe()
	done := pc.AddConn(left, nil)

	outgoing := []byte("outgoing")
	readDone := make(chan error, 1)
	go func() {
		buf := make([]byte, len(outgoing))
		_, err := io.ReadFull(right, buf)
		if err == nil && string(buf) != string(outgoing) {
			err = &unexpectedPacketError{got: string(buf), want: string(outgoing)}
		}
		readDone <- err
	}()
	if _, err := pc.WriteTo(outgoing, nil); err != nil {
		t.Fatalf("WriteTo failed: %v", err)
	}
	if err := <-readDone; err != nil {
		t.Fatalf("read outgoing packet: %v", err)
	}

	incoming := []byte("incoming")
	writeDone := make(chan error, 1)
	go func() {
		_, err := right.Write(incoming)
		writeDone <- err
	}()
	buf := make([]byte, 32)
	n, _, err := pc.ReadFrom(buf)
	if err != nil {
		t.Fatalf("ReadFrom failed: %v", err)
	}
	if got := string(buf[:n]); got != string(incoming) {
		t.Fatalf("ReadFrom = %q, want %q", got, incoming)
	}
	if err := <-writeDone; err != nil {
		t.Fatalf("write incoming packet: %v", err)
	}

	snapshot := registry.Snapshot()
	if snapshot.ActivePaths != 1 || len(snapshot.Paths) != 1 {
		t.Fatalf("active snapshot = %+v, want one path", snapshot)
	}
	path := snapshot.Paths[0]
	if path.Label != "test-metrics/path-1" || !path.Active ||
		path.BytesRead != uint64(len(incoming)) || path.BytesWritten != uint64(len(outgoing)) ||
		path.ReadOperations != 1 || path.WriteOperations != 1 ||
		path.ReadErrors != 0 || path.WriteErrors != 0 || path.WriteLatencySamples != 1 {
		t.Fatalf("active path snapshot = %+v", path)
	}

	if err := right.Close(); err != nil {
		t.Fatalf("close peer: %v", err)
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("path reader did not stop after peer close")
	}

	snapshot = registry.Snapshot()
	if snapshot.ActivePaths != 0 || len(snapshot.Paths) != 1 || snapshot.Paths[0].Active || snapshot.Paths[0].ReadErrors != 1 {
		t.Fatalf("closed snapshot = %+v, want one closed path with a read error", snapshot)
	}
}

type writeStartedConn struct {
	net.Conn
	once    sync.Once
	started chan struct{}
}

func (c *writeStartedConn) Write(p []byte) (int, error) {
	c.once.Do(func() { close(c.started) })
	return c.Conn.Write(p)
}

func TestBondedPacketConnDoesNotCountWriteErrorDuringClose(t *testing.T) {
	var registry metrics.Registry
	pc := NewBondedPacketConn("test-close-metrics")
	pc.metricRegistry = &registry

	left, right := net.Pipe()
	defer func() { _ = right.Close() }()
	conn := &writeStartedConn{Conn: left, started: make(chan struct{})}
	pc.AddConn(conn, nil)

	writeDone := make(chan error, 1)
	go func() {
		_, err := pc.WriteTo([]byte("blocked"), nil)
		writeDone <- err
	}()
	select {
	case <-conn.started:
	case <-time.After(time.Second):
		t.Fatal("path write did not start")
	}
	if err := pc.Close(); err != nil {
		t.Fatalf("close bond: %v", err)
	}
	select {
	case err := <-writeDone:
		if err == nil {
			t.Fatal("blocked write unexpectedly succeeded during close")
		}
	case <-time.After(time.Second):
		t.Fatal("blocked path write did not stop")
	}

	snapshot := registry.Snapshot()
	if snapshot.ActivePaths != 0 || snapshot.WriteOperations != 1 || snapshot.WriteErrors != 0 || snapshot.WriteLatencySamples != 0 ||
		len(snapshot.Paths) != 1 || snapshot.Paths[0].WriteOperations != 1 || snapshot.Paths[0].WriteErrors != 0 ||
		snapshot.Paths[0].WriteLatencySamples != 0 {
		t.Fatalf("close-induced write polluted error metrics: %+v", snapshot)
	}
}

func TestBondHelloRoundTrip(t *testing.T) {
	t.Parallel()

	left, right := net.Pipe()
	defer func() { _ = left.Close() }()
	defer func() { _ = right.Close() }()

	errCh := make(chan error, 1)
	go func() {
		errCh <- WriteBondHello(left, "0123456789abcdef")
	}()

	got, err := ReadBondHello(right)
	if err != nil {
		t.Fatalf("ReadBondHello failed: %v", err)
	}
	if err := <-errCh; err != nil {
		t.Fatalf("WriteBondHello failed: %v", err)
	}
	if got != "0123456789abcdef" {
		t.Fatalf("bond id = %q, want 0123456789abcdef", got)
	}
}

func TestBondHelloV1WireFormatRemainsCompatible(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer func() { _ = clientConn.Close() }()
	defer func() { _ = serverConn.Close() }()

	const want = "VKTURNBOND/1 0123456789abcdef\n"
	writeDone := make(chan error, 1)
	go func() { writeDone <- WriteBondHello(clientConn, "0123456789abcdef") }()
	buf := make([]byte, len(want))
	if _, err := io.ReadFull(serverConn, buf); err != nil {
		t.Fatalf("read V1 wire record: %v", err)
	}
	if got := string(buf); got != want {
		t.Fatalf("V1 wire record = %q, want %q", got, want)
	}
	if err := <-writeDone; err != nil {
		t.Fatalf("write V1 wire record: %v", err)
	}
}

func TestBondHelloV2RoundTripAndAck(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer func() { _ = clientConn.Close() }()
	defer func() { _ = serverConn.Close() }()

	want := BondHello{
		Version:         BondProtocolV2,
		BondID:          "0123456789abcdef",
		ExpectedPaths:   10,
		MTU:             1200,
		FECDataShards:   10,
		FECParityShards: 3,
	}
	clientDone := make(chan error, 1)
	go func() {
		if err := WriteBondHelloConfig(clientConn, want); err != nil {
			clientDone <- err
			return
		}
		clientDone <- ReadBondHelloAck(clientConn)
	}()

	got, err := ReadBondHelloConfig(serverConn)
	if err != nil {
		t.Fatalf("ReadBondHelloConfig failed: %v", err)
	}
	if got != want {
		t.Fatalf("V2 hello = %+v, want %+v", got, want)
	}
	if err := WriteBondHelloAck(serverConn); err != nil {
		t.Fatalf("WriteBondHelloAck failed: %v", err)
	}
	if err := <-clientDone; err != nil {
		t.Fatalf("client handshake failed: %v", err)
	}
}

func TestBondHelloV2RejectionRoundTrip(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer func() { _ = clientConn.Close() }()
	defer func() { _ = serverConn.Close() }()

	writeDone := make(chan error, 1)
	go func() { writeDone <- WriteBondHelloReject(serverConn, "PROFILE_MISMATCH") }()
	err := ReadBondHelloAck(clientConn)
	if !errors.Is(err, ErrBondHelloRejected) {
		t.Fatalf("rejection error = %v, want ErrBondHelloRejected", err)
	}
	var rejection *BondHelloRejectionError
	if !errors.As(err, &rejection) || rejection.Code != "PROFILE_MISMATCH" {
		t.Fatalf("rejection = %#v, want PROFILE_MISMATCH", rejection)
	}
	if err := <-writeDone; err != nil {
		t.Fatalf("write rejection: %v", err)
	}
}

func TestParseBondHelloLineRejectsInvalidProfiles(t *testing.T) {
	id := "0123456789abcdef"
	invalid := []string{
		"",
		"VKTURNBOND/3 " + id,
		"VKTURNBOND/1 " + id + " extra",
		"VKTURNBOND/2 " + id + " 1 1200 0",
		"VKTURNBOND/2 " + id + " zero 1200 0 0",
		"VKTURNBOND/2 " + id + " 0 1200 0 0",
		"VKTURNBOND/2 " + id + " 65 1200 0 0",
		"VKTURNBOND/2 " + id + " 1 1481 0 0",
		"VKTURNBOND/2 " + id + " 1 1200 10 0",
		"VKTURNBOND/2 " + id + " 1 1200 255 2",
	}
	for _, line := range invalid {
		if _, err := parseBondHelloLine(line); err == nil {
			t.Errorf("parseBondHelloLine(%q) unexpectedly succeeded", line)
		}
	}
}

func TestReadBondHelloRejectsOversizedRecord(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer func() { _ = clientConn.Close() }()
	defer func() { _ = serverConn.Close() }()

	writeDone := make(chan error, 1)
	go func() {
		_, err := clientConn.Write([]byte(strings.Repeat("x", maxBondHelloSize) + "\n"))
		writeDone <- err
	}()
	if _, err := ReadBondHelloConfig(serverConn); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("oversized hello error = %v, want size error", err)
	}
	if err := <-writeDone; err != nil {
		t.Fatalf("write oversized hello: %v", err)
	}
}

func TestValidateBondHelloTuning(t *testing.T) {
	hello := CurrentBondHello("0123456789abcdef", 4)
	if err := ValidateBondHelloTuning(hello); err != nil {
		t.Fatalf("current profile rejected: %v", err)
	}

	mismatchedMTU := hello
	if mismatchedMTU.MTU > minKCPMTU {
		mismatchedMTU.MTU--
	} else {
		mismatchedMTU.MTU++
	}
	if err := ValidateBondHelloTuning(mismatchedMTU); err == nil || !strings.Contains(err.Error(), "MTU mismatch") {
		t.Fatalf("MTU mismatch error = %v", err)
	}

	mismatchedFEC := hello
	if mismatchedFEC.FECDataShards == 1 && mismatchedFEC.FECParityShards == 1 {
		mismatchedFEC.FECDataShards = 2
	} else {
		mismatchedFEC.FECDataShards = 1
		mismatchedFEC.FECParityShards = 1
	}
	if mismatchedFEC.MTU > maxKCPMTU(mismatchedFEC.FECDataShards, mismatchedFEC.FECParityShards) {
		mismatchedFEC.MTU = maxKCPMTU(mismatchedFEC.FECDataShards, mismatchedFEC.FECParityShards)
	}
	if err := ValidateBondHelloTuning(mismatchedFEC); err == nil {
		t.Fatal("mismatched FEC profile unexpectedly accepted")
	}
}

func TestBondedPacketConnCarriesKCP(t *testing.T) {
	t.Parallel()

	clientPC := NewBondedPacketConn("client")
	defer func() { _ = clientPC.Close() }()
	serverPC := NewBondedPacketConn("server")
	defer func() { _ = serverPC.Close() }()

	clientPath1, serverPath1 := net.Pipe()
	clientPath2, serverPath2 := net.Pipe()
	clientPC.AddConn(clientPath1, nil)
	clientPC.AddConn(clientPath2, nil)
	serverPC.AddConn(serverPath1, nil)
	serverPC.AddConn(serverPath2, nil)

	serverErr := make(chan error, 1)
	go func() {
		serverSess, err := NewKCPOverPacketConn(serverPC, serverPC.RemoteAddr(), true)
		if err != nil {
			serverErr <- err
			return
		}
		defer func() { _ = serverSess.Close() }()
		_ = serverSess.SetDeadline(time.Now().Add(3 * time.Second))

		buf := make([]byte, 32)
		n, err := serverSess.Read(buf)
		if err != nil {
			serverErr <- err
			return
		}
		if string(buf[:n]) != "ping" {
			serverErr <- &unexpectedPacketError{got: string(buf[:n]), want: "ping"}
			return
		}
		_, err = serverSess.Write([]byte("pong"))
		serverErr <- err
	}()

	clientSess, err := NewKCPOverPacketConn(clientPC, clientPC.RemoteAddr(), false)
	if err != nil {
		t.Fatalf("client KCP failed: %v", err)
	}
	defer func() { _ = clientSess.Close() }()
	_ = clientSess.SetDeadline(time.Now().Add(3 * time.Second))

	if _, err := clientSess.Write([]byte("ping")); err != nil {
		t.Fatalf("client write failed: %v", err)
	}

	buf := make([]byte, 32)
	n, err := clientSess.Read(buf)
	if err != nil {
		t.Fatalf("client read failed: %v", err)
	}
	if string(buf[:n]) != "pong" {
		t.Fatalf("client read = %q, want pong", string(buf[:n]))
	}
	if err := <-serverErr; err != nil {
		t.Fatalf("server failed: %v", err)
	}
}

type unexpectedPacketError struct {
	got  string
	want string
}

func (e *unexpectedPacketError) Error() string {
	return "packet = " + e.got + ", want " + e.want
}

// --- transient vs permanent path-failure behavior (throughput regression guard) ---

type scriptedConn struct {
	writeErr   error
	writeCalls int
	closed     atomic.Bool
	closeOnce  sync.Once
	closedCh   chan struct{}
}

func newScriptedConn(writeErr error) *scriptedConn {
	return &scriptedConn{writeErr: writeErr, closedCh: make(chan struct{})}
}

func (c *scriptedConn) Read(b []byte) (int, error) {
	<-c.closedCh
	return 0, net.ErrClosed
}
func (c *scriptedConn) Write(b []byte) (int, error) {
	c.writeCalls++
	if c.writeErr != nil {
		return 0, c.writeErr
	}
	return len(b), nil
}
func (c *scriptedConn) Close() error {
	c.closed.Store(true)
	c.closeOnce.Do(func() { close(c.closedCh) })
	return nil
}
func (c *scriptedConn) LocalAddr() net.Addr                { return bondAddr("local") }
func (c *scriptedConn) RemoteAddr() net.Addr               { return bondAddr("remote") }
func (c *scriptedConn) SetDeadline(t time.Time) error      { return nil }
func (c *scriptedConn) SetReadDeadline(t time.Time) error  { return nil }
func (c *scriptedConn) SetWriteDeadline(t time.Time) error { return nil }

type timeoutErr struct{}

func (timeoutErr) Error() string   { return "i/o deadline exceeded" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }

func TestIsPermanentPathError(t *testing.T) {
	transient := []error{timeoutErr{}, errDeadlineLike(), shortWriteLike(), nil}
	for _, e := range transient {
		if isPermanentPathError(e) {
			t.Errorf("expected transient for %v", e)
		}
	}
	permanent := []error{net.ErrClosed, io.EOF, io.ErrClosedPipe, useOfClosedLike()}
	for _, e := range permanent {
		if !isPermanentPathError(e) {
			t.Errorf("expected permanent for %v", e)
		}
	}
}

func errDeadlineLike() error { return timeoutErr{} }
func shortWriteLike() error  { return ioShortWrite(1, 10) }
func useOfClosedLike() error {
	return &net.OpError{Op: "write", Err: errStr("use of closed network connection")}
}

type errStr string

func (e errStr) Error() string { return string(e) }

// A transient write error must NOT remove the path (the bug that decayed bond to
// one stream and capped throughput at ~5 Mbit).
func TestTransientWriteKeepsPath(t *testing.T) {
	pc := NewBondedPacketConn("test")
	defer func() { _ = pc.Close() }()
	c := newScriptedConn(timeoutErr{})
	pc.AddConn(c, nil)
	if pc.Count() != 1 {
		t.Fatalf("expected 1 path, got %d", pc.Count())
	}
	// Several transient-failing writes.
	for i := 0; i < 5; i++ {
		_, _ = pc.WriteTo([]byte("x"), nil)
	}
	if pc.Count() != 1 {
		t.Fatalf("transient write errors removed the path (count=%d) — regression", pc.Count())
	}
	if c.closed.Load() {
		t.Fatalf("transient write error closed the path conn — regression")
	}
}

// A permanent write error SHOULD remove the path.
func TestPermanentWriteRemovesPath(t *testing.T) {
	pc := NewBondedPacketConn("test")
	defer func() { _ = pc.Close() }()
	c := newScriptedConn(net.ErrClosed)
	pc.AddConn(c, nil)
	_, _ = pc.WriteTo([]byte("x"), nil)
	if pc.Count() != 0 {
		t.Fatalf("permanent write error did not remove the path (count=%d)", pc.Count())
	}
}

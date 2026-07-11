package tcputil

import (
	"io"
	"net"
	"testing"
	"time"
)

func TestBondedPacketConnWritesAcrossPaths(t *testing.T) {
	t.Parallel()

	pc := NewBondedPacketConn("test")
	defer pc.Close()

	left1, right1 := net.Pipe()
	defer right1.Close()
	left2, right2 := net.Pipe()
	defer right2.Close()

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
	defer pc.Close()

	left, right := net.Pipe()
	defer right.Close()
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

func TestBondHelloRoundTrip(t *testing.T) {
	t.Parallel()

	left, right := net.Pipe()
	defer left.Close()
	defer right.Close()

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

func TestBondedPacketConnCarriesKCP(t *testing.T) {
	t.Parallel()

	clientPC := NewBondedPacketConn("client")
	defer clientPC.Close()
	serverPC := NewBondedPacketConn("server")
	defer serverPC.Close()

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
		defer serverSess.Close()
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
	defer clientSess.Close()
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
	closed     bool
}

func (c *scriptedConn) Read(b []byte) (int, error) { return 0, io.EOF }
func (c *scriptedConn) Write(b []byte) (int, error) {
	c.writeCalls++
	if c.writeErr != nil {
		return 0, c.writeErr
	}
	return len(b), nil
}
func (c *scriptedConn) Close() error                       { c.closed = true; return nil }
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
	c := &scriptedConn{writeErr: timeoutErr{}}
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
	if c.closed {
		t.Fatalf("transient write error closed the path conn — regression")
	}
}

// A permanent write error SHOULD remove the path.
func TestPermanentWriteRemovesPath(t *testing.T) {
	pc := NewBondedPacketConn("test")
	defer func() { _ = pc.Close() }()
	c := &scriptedConn{writeErr: net.ErrClosed}
	pc.AddConn(c, nil)
	_, _ = pc.WriteTo([]byte("x"), nil)
	if pc.Count() != 0 {
		t.Fatalf("permanent write error did not remove the path (count=%d)", pc.Count())
	}
}

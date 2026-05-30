package tcputil

import (
	"bufio"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const bondHelloPrefix = "VKTURNBOND/1 "

type bondAddr string

func (a bondAddr) Network() string { return "bond" }
func (a bondAddr) String() string  { return string(a) }

type bondedPacket struct {
	data []byte
	addr net.Addr
}

type bondedPath struct {
	id      uint64
	conn    net.Conn
	cleanup func()
	done    chan struct{}
}

// BondedPacketConn exposes several DTLS net.Conn paths as one PacketConn.
// KCP sees one logical packet transport while writes are striped across all
// currently alive paths. This is the actual VLESS bond primitive: one smux TCP
// stream can use multiple TURN/DTLS allocations instead of being pinned to one.
type BondedPacketConn struct {
	label string

	mu       sync.RWMutex
	paths    []*bondedPath
	nextPath uint64
	nextID   uint64

	readCh    chan bondedPacket
	closeOnce sync.Once
	closed    chan struct{}

	localAddr  net.Addr
	remoteAddr net.Addr

	writeDeadline atomic.Value // time.Time
}

func NewBondedPacketConn(label string) *BondedPacketConn {
	if label == "" {
		label = "vless-bond"
	}
	return &BondedPacketConn{
		label:      label,
		readCh:     make(chan bondedPacket, 1024),
		closed:     make(chan struct{}),
		localAddr:  bondAddr(label + "/local"),
		remoteAddr: bondAddr(label + "/remote"),
	}
}

func (b *BondedPacketConn) AddConn(conn net.Conn, cleanup func()) <-chan struct{} {
	if conn == nil {
		done := make(chan struct{})
		close(done)
		return done
	}

	path := &bondedPath{
		id:      atomic.AddUint64(&b.nextID, 1),
		conn:    conn,
		cleanup: cleanup,
		done:    make(chan struct{}),
	}

	b.mu.Lock()
	select {
	case <-b.closed:
		b.mu.Unlock()
		b.closePath(path)
		return path.done
	default:
	}
	b.paths = append(b.paths, path)
	b.mu.Unlock()

	go b.readLoop(path)
	return path.done
}

func (b *BondedPacketConn) Count() int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return len(b.paths)
}

func (b *BondedPacketConn) RemoteAddr() net.Addr {
	return b.remoteAddr
}

func (b *BondedPacketConn) ReadFrom(p []byte) (int, net.Addr, error) {
	select {
	case pkt := <-b.readCh:
		// Datagram semantics: one packet per call. KCP always passes a buffer >=
		// its MTU and readLoop drops oversized packets, so copy never truncates a
		// valid KCP packet here.
		n := copy(p, pkt.data)
		return n, pkt.addr, nil
	case <-b.closed:
		return 0, nil, net.ErrClosed
	}
}

func (b *BondedPacketConn) WriteTo(p []byte, _ net.Addr) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}

	paths := b.snapshotPaths()
	if len(paths) == 0 {
		return 0, fmt.Errorf("vless bond has no active paths")
	}

	start := int(atomic.AddUint64(&b.nextPath, 1)-1) % len(paths)
	var lastErr error
	for attempt := 0; attempt < len(paths); attempt++ {
		path := paths[(start+attempt)%len(paths)]
		if deadline, ok := b.writeDeadline.Load().(time.Time); ok {
			_ = path.conn.SetWriteDeadline(deadline)
		}
		n, err := path.conn.Write(p)
		if err == nil && n == len(p) {
			return n, nil
		}
		if err == nil {
			err = ioShortWrite(n, len(p))
		}
		lastErr = err
		b.removePath(path, true)
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("vless bond has no writable paths")
	}
	return 0, lastErr
}

func (b *BondedPacketConn) Close() error {
	b.closeOnce.Do(func() {
		close(b.closed)
		for _, path := range b.snapshotPaths() {
			b.removePath(path, true)
		}
	})
	return nil
}

func (b *BondedPacketConn) LocalAddr() net.Addr {
	return b.localAddr
}

func (b *BondedPacketConn) SetDeadline(t time.Time) error {
	_ = b.SetReadDeadline(t)
	return b.SetWriteDeadline(t)
}

func (b *BondedPacketConn) SetReadDeadline(_ time.Time) error {
	return nil
}

func (b *BondedPacketConn) SetWriteDeadline(t time.Time) error {
	b.writeDeadline.Store(t)
	for _, path := range b.snapshotPaths() {
		_ = path.conn.SetWriteDeadline(t)
	}
	return nil
}

func (b *BondedPacketConn) snapshotPaths() []*bondedPath {
	b.mu.RLock()
	defer b.mu.RUnlock()
	paths := make([]*bondedPath, len(b.paths))
	copy(paths, b.paths)
	return paths
}

func (b *BondedPacketConn) readLoop(path *bondedPath) {
	defer b.removePath(path, true)

	// Read whole DTLS records (datagram framing): one Read returns exactly one
	// KCP packet. The buffer is large so a record is never split, but legitimate
	// KCP packets are <= KCPMtu.
	buf := make([]byte, 64*1024)
	for {
		n, err := path.conn.Read(buf)
		if err != nil {
			return
		}
		if n == 0 {
			continue
		}
		// Drop packets larger than the configured KCP MTU (+ header headroom):
		// delivering them would be truncated by KCP's read buffer downstream and
		// corrupt the stream. KCP recovers a dropped packet via retransmission,
		// so dropping is strictly safer than silently truncating.
		if n > KCPMtu+128 {
			continue
		}
		data := make([]byte, n)
		copy(data, buf[:n])
		select {
		case b.readCh <- bondedPacket{data: data, addr: b.remoteAddr}:
		case <-b.closed:
			return
		}
	}
}

func (b *BondedPacketConn) removePath(path *bondedPath, closeConn bool) {
	var removed bool

	b.mu.Lock()
	for i, current := range b.paths {
		if current == path {
			b.paths = append(b.paths[:i], b.paths[i+1:]...)
			removed = true
			break
		}
	}
	b.mu.Unlock()

	if removed {
		if closeConn {
			_ = path.conn.Close()
		}
		if path.cleanup != nil {
			path.cleanup()
		}
		close(path.done)
	}
}

func (b *BondedPacketConn) closePath(path *bondedPath) {
	_ = path.conn.Close()
	if path.cleanup != nil {
		path.cleanup()
	}
	close(path.done)
}

func ioShortWrite(n, want int) error {
	return fmt.Errorf("short write: wrote %d of %d bytes", n, want)
}

func WriteBondHello(conn net.Conn, bondID string) error {
	if !isValidBondID(bondID) {
		return fmt.Errorf("invalid vless bond id")
	}
	if err := conn.SetWriteDeadline(time.Now().Add(5 * time.Second)); err != nil {
		return err
	}
	_, err := conn.Write([]byte(bondHelloPrefix + bondID + "\n"))
	if resetErr := conn.SetWriteDeadline(time.Time{}); err == nil {
		err = resetErr
	}
	return err
}

// ReadBondHello reads the bond hello line. It relies on DTLS datagram framing:
// the peer sends the hello via a single WriteBondHello (its own DTLS record), so
// the first read returns exactly the hello line and no subsequent KCP data is
// consumed/lost. Do NOT coalesce the hello with other writes on the client side.
func ReadBondHello(conn net.Conn) (string, error) {
	if err := conn.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
		return "", err
	}
	line, err := bufio.NewReader(conn).ReadString('\n')
	if resetErr := conn.SetReadDeadline(time.Time{}); err == nil {
		err = resetErr
	}
	if err != nil {
		return "", err
	}

	line = strings.TrimSpace(line)
	if !strings.HasPrefix(line, bondHelloPrefix) {
		return "", errors.New("missing vless bond hello")
	}
	bondID := strings.TrimPrefix(line, bondHelloPrefix)
	if !isValidBondID(bondID) {
		return "", fmt.Errorf("invalid vless bond id")
	}
	return bondID, nil
}

func isValidBondID(value string) bool {
	if len(value) < 8 || len(value) > 64 {
		return false
	}
	for _, r := range value {
		if r >= 'a' && r <= 'f' {
			continue
		}
		if r >= 'A' && r <= 'F' {
			continue
		}
		if r >= '0' && r <= '9' {
			continue
		}
		return false
	}
	return true
}

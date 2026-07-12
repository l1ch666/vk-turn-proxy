package tcputil

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	bondHelloV1Token = "VKTURNBOND/1"
	bondHelloV2Token = "VKTURNBOND/2"
	bondHelloV2Ack   = "VKTURNBOND/2 OK\n"
	maxBondHelloSize = 256
	MaxBondPaths     = 64
	BondProtocolV1   = 1
	BondProtocolV2   = 2
)

// BondHello is the per-path control record sent before KCP traffic. V1 carries
// only a bond ID. V2 also carries the wire-critical KCP profile and is confirmed
// by the server before either side starts KCP.
type BondHello struct {
	Version         int
	BondID          string
	ExpectedPaths   int
	MTU             int
	FECDataShards   int
	FECParityShards int
}

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

		// CRITICAL: only remove a path on a PERMANENT failure (the connection is
		// closed/broken). A transient write error — e.g. a write-deadline timeout
		// (pion DTLS returns errDeadlineExceeded) or momentary relay backpressure —
		// must NOT tear the path down: doing so made bond decay to a single path
		// under normal loss, collapsing throughput to one stream (~5 Mbit) and
		// causing reconnect churn. On a transient error we keep the path and just
		// try the next one for THIS packet; KCP retransmits anything truly lost.
		if err == nil {
			err = ioShortWrite(n, len(p))
		}
		lastErr = err
		if isPermanentPathError(err) {
			b.removePath(path, true)
		}
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("vless bond has no writable paths")
	}
	// Report the packet as "sent" for a transient miss: KCP owns reliability and
	// will retransmit. Returning an error here would propagate up and can abort the
	// KCP session on what is only a momentary, recoverable condition.
	if !isPermanentPathError(lastErr) {
		return len(p), nil
	}
	return 0, lastErr
}

// isPermanentPathError reports whether a path write error means the underlying
// connection is dead (so the path should be removed) rather than a transient,
// recoverable condition (timeout/backpressure/short write) that KCP can ride out.
func isPermanentPathError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, net.ErrClosed) || errors.Is(err, io.ErrClosedPipe) || errors.Is(err, io.EOF) {
		return true
	}
	// Timeouts are explicitly transient.
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return false
	}
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "deadline exceeded"),
		strings.Contains(msg, "timeout"),
		strings.Contains(msg, "temporarily"),
		strings.Contains(msg, "short write"),
		strings.Contains(msg, "buffer"):
		return false
	case strings.Contains(msg, "closed"),
		strings.Contains(msg, "broken pipe"),
		strings.Contains(msg, "reset"),
		strings.Contains(msg, "use of closed"):
		return true
	}
	// Unknown errors: treat as transient (keep the path). A genuinely dead path
	// will surface a closed/EOF on its readLoop and be removed there anyway.
	return false
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
	return WriteBondHelloConfig(conn, BondHello{Version: BondProtocolV1, BondID: bondID})
}

// CurrentBondHello returns a V2 hello with the active wire-critical KCP profile.
func CurrentBondHello(bondID string, expectedPaths int) BondHello {
	dataShards, parityShards := FECShards()
	return BondHello{
		Version:         BondProtocolV2,
		BondID:          bondID,
		ExpectedPaths:   expectedPaths,
		MTU:             KCPMtu,
		FECDataShards:   dataShards,
		FECParityShards: parityShards,
	}
}

// WriteBondHelloConfig writes one standalone DTLS control record. KCP traffic
// must not be coalesced into the same record.
func WriteBondHelloConfig(conn net.Conn, hello BondHello) error {
	if err := validateBondHello(hello); err != nil {
		return err
	}
	var line string
	switch hello.Version {
	case BondProtocolV1:
		line = fmt.Sprintf("%s %s\n", bondHelloV1Token, hello.BondID)
	case BondProtocolV2:
		line = fmt.Sprintf("%s %s %d %d %d %d\n",
			bondHelloV2Token,
			hello.BondID,
			hello.ExpectedPaths,
			hello.MTU,
			hello.FECDataShards,
			hello.FECParityShards,
		)
	default:
		return fmt.Errorf("unsupported vless bond protocol version %d", hello.Version)
	}
	return writeBondControlLine(conn, line)
}

// WriteBondHelloAck confirms that the server accepted a V2 profile.
func WriteBondHelloAck(conn net.Conn) error {
	return writeBondControlLine(conn, bondHelloV2Ack)
}

// ReadBondHello reads the bond hello line. It relies on DTLS datagram framing:
// the peer sends the hello via a single WriteBondHello (its own DTLS record), so
// the first read returns exactly the hello line and no subsequent KCP data is
// consumed/lost. Do NOT coalesce the hello with other writes on the client side.
func ReadBondHello(conn net.Conn) (string, error) {
	hello, err := ReadBondHelloConfig(conn)
	if err != nil {
		return "", err
	}
	return hello.BondID, nil
}

// ReadBondHelloConfig accepts both the legacy V1 record and the V2 profile.
func ReadBondHelloConfig(conn net.Conn) (BondHello, error) {
	line, err := readBondControlLine(conn, 10*time.Second)
	if err != nil {
		return BondHello{}, err
	}
	return parseBondHelloLine(line)
}

// ReadBondHelloAck waits for the server to confirm a V2 profile.
func ReadBondHelloAck(conn net.Conn) error {
	line, err := readBondControlLine(conn, 5*time.Second)
	if err != nil {
		return err
	}
	if line != strings.TrimSpace(bondHelloV2Ack) {
		return fmt.Errorf("unexpected vless bond V2 acknowledgement %q", line)
	}
	return nil
}

// ValidateBondHelloTuning checks that a received V2 wire profile matches the
// server's local MTU/FEC settings. V1 has no profile and remains accepted.
func ValidateBondHelloTuning(hello BondHello) error {
	if err := validateBondHello(hello); err != nil {
		return err
	}
	if hello.Version == BondProtocolV1 {
		return nil
	}
	dataShards, parityShards := FECShards()
	if hello.MTU != KCPMtu {
		return fmt.Errorf("vless bond KCP MTU mismatch: client=%d server=%d", hello.MTU, KCPMtu)
	}
	if hello.FECDataShards != dataShards || hello.FECParityShards != parityShards {
		return fmt.Errorf("vless bond FEC mismatch: client=%d:%d server=%d:%d",
			hello.FECDataShards,
			hello.FECParityShards,
			dataShards,
			parityShards,
		)
	}
	return nil
}

func parseBondHelloLine(line string) (BondHello, error) {
	fields := strings.Fields(line)
	if len(fields) == 0 {
		return BondHello{}, errors.New("missing vless bond hello")
	}
	switch fields[0] {
	case bondHelloV1Token:
		if len(fields) != 2 {
			return BondHello{}, fmt.Errorf("invalid vless bond V1 hello field count")
		}
		hello := BondHello{Version: BondProtocolV1, BondID: fields[1]}
		return hello, validateBondHello(hello)
	case bondHelloV2Token:
		if len(fields) != 6 {
			return BondHello{}, fmt.Errorf("invalid vless bond V2 hello field count")
		}
		values := make([]int, 4)
		for i, raw := range fields[2:] {
			value, err := strconv.Atoi(raw)
			if err != nil {
				return BondHello{}, fmt.Errorf("invalid vless bond V2 numeric field %q: %w", raw, err)
			}
			values[i] = value
		}
		hello := BondHello{
			Version:         BondProtocolV2,
			BondID:          fields[1],
			ExpectedPaths:   values[0],
			MTU:             values[1],
			FECDataShards:   values[2],
			FECParityShards: values[3],
		}
		return hello, validateBondHello(hello)
	default:
		return BondHello{}, fmt.Errorf("unsupported vless bond protocol %q", fields[0])
	}
}

func validateBondHello(hello BondHello) error {
	if !isValidBondID(hello.BondID) {
		return fmt.Errorf("invalid vless bond id")
	}
	switch hello.Version {
	case BondProtocolV1:
		if hello.ExpectedPaths != 0 || hello.MTU != 0 || hello.FECDataShards != 0 || hello.FECParityShards != 0 {
			return fmt.Errorf("vless bond V1 hello must not contain a transport profile")
		}
	case BondProtocolV2:
		if hello.ExpectedPaths < 1 || hello.ExpectedPaths > MaxBondPaths {
			return fmt.Errorf("vless bond expected paths must be in 1..%d", MaxBondPaths)
		}
		if err := validateFECShards(hello.FECDataShards, hello.FECParityShards); err != nil {
			return fmt.Errorf("invalid vless bond FEC profile: %w", err)
		}
		maxMTU := maxKCPMTU(hello.FECDataShards, hello.FECParityShards)
		if hello.MTU < minKCPMTU || hello.MTU > maxMTU {
			return fmt.Errorf("vless bond KCP MTU must be in %d..%d", minKCPMTU, maxMTU)
		}
	default:
		return fmt.Errorf("unsupported vless bond protocol version %d", hello.Version)
	}
	return nil
}

func writeBondControlLine(conn net.Conn, line string) error {
	if len(line) > maxBondHelloSize || !strings.HasSuffix(line, "\n") {
		return fmt.Errorf("invalid vless bond control record length")
	}
	if err := conn.SetWriteDeadline(time.Now().Add(5 * time.Second)); err != nil {
		return err
	}
	n, err := conn.Write([]byte(line))
	if err == nil && n != len(line) {
		err = ioShortWrite(n, len(line))
	}
	if resetErr := conn.SetWriteDeadline(time.Time{}); err == nil {
		err = resetErr
	}
	return err
}

func readBondControlLine(conn net.Conn, timeout time.Duration) (string, error) {
	if err := conn.SetReadDeadline(time.Now().Add(timeout)); err != nil {
		return "", err
	}
	reader := bufio.NewReaderSize(io.LimitReader(conn, maxBondHelloSize+1), maxBondHelloSize+1)
	line, err := reader.ReadString('\n')
	if resetErr := conn.SetReadDeadline(time.Time{}); err == nil {
		err = resetErr
	}
	if len(line) > maxBondHelloSize {
		return "", fmt.Errorf("vless bond control record exceeds %d bytes", maxBondHelloSize)
	}
	if err != nil {
		return "", err
	}
	if !strings.HasSuffix(line, "\n") {
		return "", fmt.Errorf("unterminated vless bond control record")
	}
	return strings.TrimSpace(line), nil
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

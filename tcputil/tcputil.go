package tcputil

import (
	"flag"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/xtaci/kcp-go/v5"
	"github.com/xtaci/smux"
)

// Tunable KCP/smux parameters. Defaults match the previous hardcoded values, so
// behavior is unchanged unless overridden. Override precedence: -flag > env >
// default. Env names: VK_TURN_KCP_WND / _INTERVAL / _NODELAY / _RESEND / _NC /
// _MTU, VK_TURN_SMUX_RECVBUF / _STREAMBUF. Register flags via RegisterTuningFlags
// before flag.Parse(). See docs/TUNING.md for a measurement-driven methodology.
var (
	KCPWindow     = envInt("VK_TURN_KCP_WND", 256)
	KCPInterval   = envInt("VK_TURN_KCP_INTERVAL", 20)
	KCPNoDelay    = envInt("VK_TURN_KCP_NODELAY", 1)
	KCPResend     = envInt("VK_TURN_KCP_RESEND", 2)
	KCPNC         = envInt("VK_TURN_KCP_NC", 1)
	KCPMtu        = envInt("VK_TURN_KCP_MTU", 1200)
	SmuxRecvBuf   = envInt("VK_TURN_SMUX_RECVBUF", 4*1024*1024)
	SmuxStreamBuf = envInt("VK_TURN_SMUX_STREAMBUF", 1*1024*1024)

	// KCP Reed-Solomon forward error correction (dataShards:parityShards). 0:0 =
	// OFF (default, unchanged). FEC reconstructs lost packets WITHOUT waiting for a
	// retransmit round-trip — a big win on high-RTT lossy TURN paths where
	// TCP-over-TCP retransmits otherwise collapse throughput. MUST match on client
	// and server. The Android app sets VK_TURN_KCP_FEC=10:3 on both ends (client
	// process env + server via vk-turn-control.sh) when "KCP FEC" is enabled.
	kcpDataShards, kcpParityShards = parseFEC(os.Getenv("VK_TURN_KCP_FEC"))
)

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return def
}

// parseFEC parses "data:parity" (e.g. "10:3"). Returns 0,0 (FEC off) on any
// malformed/empty input or non-positive values.
func parseFEC(v string) (int, int) {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0, 0
	}
	parts := strings.SplitN(v, ":", 2)
	if len(parts) != 2 {
		return 0, 0
	}
	d, errD := strconv.Atoi(strings.TrimSpace(parts[0]))
	p, errP := strconv.Atoi(strings.TrimSpace(parts[1]))
	if errD != nil || errP != nil || d <= 0 || p <= 0 {
		return 0, 0
	}
	return d, p
}

// FECShards returns the configured Reed-Solomon (dataShards, parityShards).
func FECShards() (int, int) { return kcpDataShards, kcpParityShards }

// RegisterTuningFlags binds the KCP/smux knobs to the default flag set. Call it
// before flag.Parse() in each binary. Flag defaults are the env-or-default values,
// so a flag overrides env, env overrides the built-in default.
func RegisterTuningFlags() {
	flag.IntVar(&KCPWindow, "kcp-window", KCPWindow, "KCP send/recv window in packets (higher = more in-flight for high-RTT TURN paths)")
	flag.IntVar(&KCPInterval, "kcp-interval", KCPInterval, "KCP flush interval in ms (lower = lower latency, more CPU)")
	flag.IntVar(&KCPMtu, "kcp-mtu", KCPMtu, "KCP MTU in bytes (must fit inside DTLS+TURN; keep <= inner tunnel MTU)")
	flag.IntVar(&SmuxRecvBuf, "smux-recvbuf", SmuxRecvBuf, "smux max receive buffer in bytes")
	flag.IntVar(&SmuxStreamBuf, "smux-streambuf", SmuxStreamBuf, "smux max per-stream buffer in bytes")
	flag.Func("kcp-fec", "KCP Reed-Solomon FEC as data:parity (e.g. 10:3; empty/0:0 = off). MUST match server.", func(v string) error {
		kcpDataShards, kcpParityShards = parseFEC(v)
		return nil
	})
}

// TuningSummary returns a one-line human-readable summary of active KCP/smux
// tuning, for startup logging.
func TuningSummary() string {
	fec := "off"
	if kcpDataShards > 0 && kcpParityShards > 0 {
		fec = strconv.Itoa(kcpDataShards) + ":" + strconv.Itoa(kcpParityShards)
	}
	return "kcp[wnd=" + strconv.Itoa(KCPWindow) +
		" interval=" + strconv.Itoa(KCPInterval) +
		" mtu=" + strconv.Itoa(KCPMtu) +
		" fec=" + fec + "]"
}

// DtlsPacketConn wraps a net.Conn (DTLS) as a net.PacketConn for KCP.
// Each DTLS Read/Write preserves message boundaries (datagram semantics).
type DtlsPacketConn struct {
	conn net.Conn
}

func NewDtlsPacketConn(conn net.Conn) *DtlsPacketConn {
	return &DtlsPacketConn{conn: conn}
}

func (d *DtlsPacketConn) ReadFrom(b []byte) (int, net.Addr, error) {
	n, err := d.conn.Read(b)
	return n, d.conn.RemoteAddr(), err
}

func (d *DtlsPacketConn) WriteTo(b []byte, _ net.Addr) (int, error) {
	return d.conn.Write(b)
}

func (d *DtlsPacketConn) Close() error {
	return d.conn.Close()
}

func (d *DtlsPacketConn) LocalAddr() net.Addr {
	return d.conn.LocalAddr()
}

func (d *DtlsPacketConn) SetDeadline(t time.Time) error {
	return d.conn.SetDeadline(t)
}

func (d *DtlsPacketConn) SetReadDeadline(t time.Time) error {
	return d.conn.SetReadDeadline(t)
}

func (d *DtlsPacketConn) SetWriteDeadline(t time.Time) error {
	return d.conn.SetWriteDeadline(t)
}

// NewKCPOverPacketConn creates a KCP session over a packet transport.
// isServer: true for server-side (listener), false for client-side (dialer).
func NewKCPOverPacketConn(pc net.PacketConn, remote net.Addr, isServer bool) (*kcp.UDPSession, error) {
	return newKCPOverPacketConn(pc, remote, isServer, KCPWindow)
}

// NewKCPOverPacketConnBonded creates a KCP session whose window is scaled for a
// bonded transport carrying pathCount parallel TURN/DTLS paths. A bond aggregates
// the bandwidth-delay product of all paths into ONE KCP pipe, so a single-path
// window would cap the combined throughput. The window scales with pathCount,
// bounded to avoid pathological memory use.
func NewKCPOverPacketConnBonded(pc net.PacketConn, remote net.Addr, isServer bool, pathCount int) (*kcp.UDPSession, error) {
	if pathCount < 1 {
		pathCount = 1
	}
	wnd := KCPWindow * pathCount
	const maxWnd = 8192 // cap: ~8192*1200B ≈ 9.4MB of in-flight per direction
	if wnd > maxWnd {
		wnd = maxWnd
	}
	return newKCPOverPacketConn(pc, remote, isServer, wnd)
}

func newKCPOverPacketConn(pc net.PacketConn, remote net.Addr, isServer bool, window int) (*kcp.UDPSession, error) {
	block, err := kcp.NewNoneBlockCrypt(nil) // DTLS already encrypts
	if err != nil {
		return nil, err
	}

	var sess *kcp.UDPSession

	// Reed-Solomon FEC shards (0,0 = off). Must match on both ends.
	dataShards, parityShards := kcpDataShards, kcpParityShards

	if isServer {
		// Server: listen on the PacketConn and accept one session
		var listener *kcp.Listener
		listener, err = kcp.ServeConn(block, dataShards, parityShards, pc)
		if err != nil {
			return nil, err
		}
		if err = listener.SetDeadline(time.Now().Add(30 * time.Second)); err != nil {
			return nil, err
		}
		sess, err = listener.AcceptKCP()
		if err != nil {
			return nil, err
		}
	} else {
		// Client: dial through the PacketConn
		sess, err = kcp.NewConn2(remote, block, dataShards, parityShards, pc)
		if err != nil {
			return nil, err
		}
	}

	// Tune KCP for TURN tunnel:
	// - NoDelay mode for lower latency
	// - Window sizes suitable for ~5Mbit/s
	sess.SetNoDelay(KCPNoDelay, KCPInterval, KCPResend, KCPNC)
	sess.SetWindowSize(window, window)
	sess.SetMtu(KCPMtu) // must fit inside DTLS+TURN; keep <= inner tunnel MTU
	sess.SetACKNoDelay(true)

	return sess, nil
}

// NewKCPOverDTLS creates a KCP session over a DTLS connection.
// isServer: true for server-side (listener), false for client-side (dialer).
func NewKCPOverDTLS(dtlsConn net.Conn, isServer bool) (*kcp.UDPSession, error) {
	return NewKCPOverPacketConn(NewDtlsPacketConn(dtlsConn), dtlsConn.RemoteAddr(), isServer)
}

// DefaultSmuxConfig returns smux config tuned for TURN tunnel.
func DefaultSmuxConfig() *smux.Config {
	cfg := smux.DefaultConfig()
	cfg.MaxReceiveBuffer = SmuxRecvBuf
	cfg.MaxStreamBuffer = SmuxStreamBuf
	cfg.KeepAliveInterval = 10 * time.Second
	cfg.KeepAliveTimeout = 30 * time.Second
	return cfg
}

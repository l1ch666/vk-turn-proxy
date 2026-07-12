package tcputil

import (
	"errors"
	"flag"
	"fmt"
	"math"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/xtaci/kcp-go/v5"
	"github.com/xtaci/smux"
)

const (
	maxKCPWindow       = 8192
	minKCPMTU          = 50
	kcpXmitBufferSize  = 1500
	kcpCryptHeaderSize = 20
	kcpFECHeaderSize   = 8
)

var tuningEnvErrors []error

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
	kcpDataShards, kcpParityShards = envFEC("VK_TURN_KCP_FEC")
)

func envInt(key string, def int) int {
	v, ok := os.LookupEnv(key)
	if !ok || strings.TrimSpace(v) == "" {
		return def
	}
	n, err := parseEnvInt(v)
	if err != nil {
		tuningEnvErrors = append(tuningEnvErrors, fmt.Errorf("%s must be an integer: %w", key, err))
		return def
	}
	return n
}

func parseEnvInt(v string) (int, error) {
	return strconv.Atoi(strings.TrimSpace(v))
}

func envFEC(key string) (int, int) {
	v, ok := os.LookupEnv(key)
	if !ok || strings.TrimSpace(v) == "" {
		return 0, 0
	}
	d, p, err := parseFEC(v)
	if err != nil {
		tuningEnvErrors = append(tuningEnvErrors, fmt.Errorf("%s: %w", key, err))
		return 0, 0
	}
	return d, p
}

// parseFEC parses "data:parity" (for example "10:3"). Empty and "0:0"
// disable FEC; every other value must describe a valid Reed-Solomon profile.
func parseFEC(v string) (int, int, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0, 0, nil
	}
	parts := strings.Split(v, ":")
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("expected data:parity")
	}
	d, errD := strconv.Atoi(strings.TrimSpace(parts[0]))
	p, errP := strconv.Atoi(strings.TrimSpace(parts[1]))
	if errD != nil || errP != nil {
		return 0, 0, fmt.Errorf("data and parity shards must be integers")
	}
	if d == 0 && p == 0 {
		return 0, 0, nil
	}
	if d <= 0 || p <= 0 {
		return 0, 0, fmt.Errorf("data and parity shards must both be positive, or both zero")
	}
	if err := validateFECShards(d, p); err != nil {
		return 0, 0, err
	}
	return d, p, nil
}

func validateFECShards(dataShards, parityShards int) error {
	if dataShards == 0 && parityShards == 0 {
		return nil
	}
	if dataShards <= 0 || parityShards <= 0 {
		return fmt.Errorf("data and parity shards must both be positive, or both zero")
	}
	if dataShards > 256 || parityShards > 256 || dataShards > 256-parityShards {
		return fmt.Errorf("data and parity shards must total at most 256")
	}
	return nil
}

func maxKCPMTU(dataShards, parityShards int) int {
	maxMTU := kcpXmitBufferSize - kcpCryptHeaderSize
	if dataShards > 0 && parityShards > 0 {
		maxMTU -= kcpFECHeaderSize
	}
	return maxMTU
}

// FECShards returns the configured Reed-Solomon (dataShards, parityShards).
func FECShards() (int, int) { return kcpDataShards, kcpParityShards }

// RegisterTuningFlags binds the KCP/smux knobs to the default flag set. Call it
// before flag.Parse() in each binary. Flag defaults are the env-or-default values,
// so a flag overrides env, env overrides the built-in default.
func RegisterTuningFlags() {
	registerTuningFlags(flag.CommandLine)
}

func registerTuningFlags(fs *flag.FlagSet) {
	fs.IntVar(&KCPWindow, "kcp-window", KCPWindow, "KCP send/recv window in packets (higher = more in-flight for high-RTT TURN paths)")
	fs.IntVar(&KCPInterval, "kcp-interval", KCPInterval, "KCP flush interval in ms (lower = lower latency, more CPU)")
	fs.IntVar(&KCPNoDelay, "kcp-nodelay", KCPNoDelay, "KCP nodelay mode: 0 or 1")
	fs.IntVar(&KCPResend, "kcp-resend", KCPResend, "KCP fast-resend threshold (0 disables)")
	fs.IntVar(&KCPNC, "kcp-nc", KCPNC, "KCP congestion control: 0 enabled, 1 disabled")
	fs.IntVar(&KCPMtu, "kcp-mtu", KCPMtu, "KCP MTU in bytes (must fit inside DTLS+TURN; keep <= inner tunnel MTU)")
	fs.IntVar(&SmuxRecvBuf, "smux-recvbuf", SmuxRecvBuf, "smux max receive buffer in bytes")
	fs.IntVar(&SmuxStreamBuf, "smux-streambuf", SmuxStreamBuf, "smux max per-stream buffer in bytes")
	fs.Func("kcp-fec", "KCP Reed-Solomon FEC as data:parity (e.g. 10:3; empty/0:0 = off). MUST match server.", func(v string) error {
		dataShards, parityShards, err := parseFEC(v)
		if err != nil {
			return err
		}
		kcpDataShards, kcpParityShards = dataShards, parityShards
		return nil
	})
}

type tuningValues struct {
	window       int
	interval     int
	nodelay      int
	resend       int
	nc           int
	mtu          int
	smuxRecv     int
	smuxStream   int
	dataShards   int
	parityShards int
}

func currentTuningValues() tuningValues {
	return tuningValues{
		window:       KCPWindow,
		interval:     KCPInterval,
		nodelay:      KCPNoDelay,
		resend:       KCPResend,
		nc:           KCPNC,
		mtu:          KCPMtu,
		smuxRecv:     SmuxRecvBuf,
		smuxStream:   SmuxStreamBuf,
		dataShards:   kcpDataShards,
		parityShards: kcpParityShards,
	}
}

// ValidateTuning rejects values that kcp-go/smux would otherwise silently
// clamp, ignore, overflow, or accept only to fail later on the packet path.
func ValidateTuning() error {
	errs := append([]error(nil), tuningEnvErrors...)
	if err := validateTuning(currentTuningValues()); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

func validateTuning(v tuningValues) error {
	var errs []error
	if v.window < 1 || v.window > maxKCPWindow {
		errs = append(errs, fmt.Errorf("kcp-window must be in 1..%d", maxKCPWindow))
	}
	if v.interval < 10 || v.interval > 5000 {
		errs = append(errs, fmt.Errorf("kcp-interval must be in 10..5000 ms"))
	}
	if v.nodelay != 0 && v.nodelay != 1 {
		errs = append(errs, fmt.Errorf("kcp-nodelay must be 0 or 1"))
	}
	if v.resend < 0 || int64(v.resend) > math.MaxInt32 {
		errs = append(errs, fmt.Errorf("kcp-resend must be in 0..%d", math.MaxInt32))
	}
	if v.nc != 0 && v.nc != 1 {
		errs = append(errs, fmt.Errorf("kcp-nc must be 0 or 1"))
	}
	if err := validateFECShards(v.dataShards, v.parityShards); err != nil {
		errs = append(errs, fmt.Errorf("kcp-fec: %w", err))
	}
	maxMTU := maxKCPMTU(v.dataShards, v.parityShards)
	if v.mtu < minKCPMTU || v.mtu > maxMTU {
		errs = append(errs, fmt.Errorf("kcp-mtu must be in %d..%d for the selected FEC profile", minKCPMTU, maxMTU))
	}
	if v.smuxRecv < 1 || int64(v.smuxRecv) > math.MaxInt32 {
		errs = append(errs, fmt.Errorf("smux-recvbuf must be in 1..%d", math.MaxInt32))
	}
	if v.smuxStream < 1 || int64(v.smuxStream) > math.MaxInt32 {
		errs = append(errs, fmt.Errorf("smux-streambuf must be in 1..%d", math.MaxInt32))
	} else if v.smuxStream > v.smuxRecv {
		errs = append(errs, fmt.Errorf("smux-streambuf must not exceed smux-recvbuf"))
	}
	return errors.Join(errs...)
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
		" nodelay=" + strconv.Itoa(KCPNoDelay) +
		" resend=" + strconv.Itoa(KCPResend) +
		" nc=" + strconv.Itoa(KCPNC) +
		" mtu=" + strconv.Itoa(KCPMtu) +
		" fec=" + fec + "] smux[recv=" + strconv.Itoa(SmuxRecvBuf) +
		" stream=" + strconv.Itoa(SmuxStreamBuf) + "]"
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
	return newKCPOverPacketConn(pc, remote, isServer, BondedKCPWindow(pathCount))
}

// BondedKCPWindow returns the bounded KCP window for an aggregate path count.
// Division before multiplication prevents an attacker-controlled or malformed
// path count from overflowing int before the memory-safety cap is applied.
func BondedKCPWindow(pathCount int) int {
	return bondedKCPWindow(KCPWindow, pathCount)
}

func bondedKCPWindow(baseWindow, pathCount int) int {
	if baseWindow < 1 {
		baseWindow = 1
	}
	if baseWindow >= maxKCPWindow {
		return maxKCPWindow
	}
	if pathCount < 1 {
		pathCount = 1
	}
	if pathCount > maxKCPWindow/baseWindow {
		return maxKCPWindow
	}
	return baseWindow * pathCount
}

func newKCPOverPacketConn(pc net.PacketConn, remote net.Addr, isServer bool, window int) (*kcp.UDPSession, error) {
	if err := validateTuning(currentTuningValues()); err != nil {
		return nil, fmt.Errorf("invalid transport tuning: %w", err)
	}
	if window < 1 || window > maxKCPWindow {
		return nil, fmt.Errorf("KCP window must be in 1..%d", maxKCPWindow)
	}

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
	if !sess.SetMtu(KCPMtu) {
		_ = sess.Close()
		return nil, fmt.Errorf("kcp rejected MTU %d", KCPMtu)
	}
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

package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/l1ch666/vk-turn-proxy/v2/diagnostics"
	"github.com/l1ch666/vk-turn-proxy/v2/metrics"
	"github.com/l1ch666/vk-turn-proxy/v2/sessionauth"
	"github.com/l1ch666/vk-turn-proxy/v2/tcputil"
	"github.com/pion/dtls/v3"
	"github.com/xtaci/smux"
)

func main() {
	listen := flag.String("listen", "0.0.0.0:56000", "listen on ip:port")
	connect := flag.String("connect", "", "connect to ip:port")
	vlessMode := flag.Bool("vless", false, "VLESS mode: forward TCP connections (for VLESS) instead of UDP packets")
	vlessBond := flag.Bool("vless-bond", false, "VLESS bond mode: packet-level multipath across TURN/DTLS streams; requires -vless")
	wrap := flag.Bool("wrap", false, "unsupported compatibility flag; exits with an error")
	wrapKey := flag.String("wrap-key", "", "unsupported compatibility flag; exits with an error")
	genWrapKey := flag.Bool("gen-wrap-key", false, "unsupported compatibility flag; exits with an error")
	maxConnections := flag.Int("max-connections", defaultMaxTransportConnections, "maximum active DTLS transport connections")
	maxConnectionsPerIP := flag.Int("max-connections-per-ip", defaultMaxTransportConnectionsPerIP, "maximum active DTLS transport connections per source IP")
	maxBackendConnections := flag.Int("max-backend-connections", defaultMaxBackendConnections, "maximum active VLESS backend streams across all sessions")
	maxStreamsPerSession := flag.Int("max-streams-per-session", defaultMaxStreamsPerSession, "maximum active VLESS streams in one smux session")
	dtlsCertificateFile := flag.String("dtls-cert-file", "", "PEM server certificate for a persistent DTLS identity (requires -dtls-key-file)")
	dtlsKeyFile := flag.String("dtls-key-file", "", "PEM private key for the DTLS server certificate (requires -dtls-cert-file)")
	clientAuthTokenFile := flag.String("client-auth-token-file", "", "private 64-hex-character token file used to authenticate clients (created with mode 0600 when missing)")
	unsafeAllowUnauthenticatedClients := flag.Bool("unsafe-allow-unauthenticated-clients", false, "disable client authentication; exposes the configured backend to any DTLS client")
	diagnosticOptions := diagnostics.RegisterFlags(flag.CommandLine)
	tcputil.RegisterTuningFlags()
	flag.Parse()
	if err := validateServerCompatibilityFlags(*wrap, *wrapKey, *genWrapKey); err != nil {
		log.Fatalf("%s", err)
	}
	connectionLimiter, limitErr := newConnectionLimiter(*maxConnections, *maxConnectionsPerIP)
	if limitErr != nil {
		log.Fatalf("invalid connection limits: %s", limitErr)
	}
	backendGate, backendLimitErr := newBackendGate(*maxBackendConnections, *maxStreamsPerSession, &metrics.Process)
	if backendLimitErr != nil {
		log.Fatalf("invalid backend limits: %s", backendLimitErr)
	}
	if err := tcputil.ValidateTuning(); err != nil {
		log.Fatalf("invalid transport tuning: %s", err)
	}
	log.Printf("tuning: %s", tcputil.TuningSummary())
	log.Printf("connection limits: total=%d per-ip=%d", *maxConnections, *maxConnectionsPerIP)
	log.Printf("backend limits: total=%d per-session=%d", *maxBackendConnections, *maxStreamsPerSession)
	diagnosticConfig, diagnosticErr := diagnosticOptions.Config()
	if diagnosticErr != nil {
		log.Fatalf("invalid diagnostics configuration: %s", diagnosticErr)
	}
	if err := validateServerVLESSFlags(*vlessMode, *vlessBond); err != nil {
		log.Fatalf("%s", err)
	}
	dtlsIdentity, identityErr := loadServerDTLSIdentity(*dtlsCertificateFile, *dtlsKeyFile)
	if identityErr != nil {
		log.Fatalf("invalid DTLS identity: %s", identityErr)
	}
	log.Printf("DTLS server certificate SHA-256 fingerprint: %s", dtlsIdentity.fingerprint)
	if dtlsIdentity.ephemeral {
		log.Printf("WARNING: using an ephemeral DTLS identity; the fingerprint changes on restart (set -dtls-cert-file and -dtls-key-file for a persistent identity)")
	}
	var clientAuthToken *sessionauth.Token
	if *unsafeAllowUnauthenticatedClients {
		if *clientAuthTokenFile != "" {
			log.Fatalf("-client-auth-token-file and -unsafe-allow-unauthenticated-clients cannot be used together")
		}
		log.Printf("WARNING: DTLS client authentication is disabled by explicit unsafe override")
	} else {
		token, created, tokenErr := sessionauth.LoadOrCreateTokenFile(*clientAuthTokenFile)
		if tokenErr != nil {
			log.Fatalf("invalid client authentication token: %s", tokenErr)
		}
		clientAuthToken = &token
		if created {
			log.Printf("created client authentication token file %s; copy it to clients over an authenticated channel", *clientAuthTokenFile)
		}
		log.Printf("DTLS client authentication: required")
	}
	log.Printf("vless mode: %s", enabledText(*vlessMode))
	if *vlessMode {
		log.Printf("vless bond: %s", enabledText(*vlessBond))
		if *vlessBond {
			log.Printf("vless bond semantics: packet-level multipath over TURN/DTLS paths")
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	signalChan := make(chan os.Signal, 1)
	signal.Notify(signalChan, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		<-signalChan
		log.Printf("Terminating...\n")
		cancel()
		<-signalChan
		log.Fatalf("Exit...\n")
	}()
	if _, err := diagnostics.Start(ctx, diagnosticConfig, &metrics.Process); err != nil {
		log.Fatalf("start diagnostics: %s", err)
	}

	addr, err := net.ResolveUDPAddr("udp", *listen)
	if err != nil {
		panic(err)
	}
	if len(*connect) == 0 {
		log.Panicf("server address is required")
	}
	//
	// Everything below is the pion-DTLS API! Thanks for using it ❤️.
	//

	// Connect to a DTLS server
	listener, err := dtls.ListenWithOptions(
		"udp",
		addr,
		dtls.WithCertificates(dtlsIdentity.certificate),
		dtls.WithExtendedMasterSecret(dtls.RequireExtendedMasterSecret),
		dtls.WithCipherSuites(dtlsIdentity.cipherSuite),
		dtls.WithConnectionIDGenerator(dtls.RandomCIDGenerator(8)),
	)
	if err != nil {
		panic(err)
	}
	context.AfterFunc(ctx, func() {
		_ = listener.Close()
	})

	fmt.Println("Listening")
	var bondManager *vlessBondManager
	if *vlessMode && *vlessBond {
		bondManager = newVLESSBondManagerWithGate(*connect, backendGate)
	}

	wg1 := sync.WaitGroup{}
	var connectionRejections atomic.Uint64
	for {
		select {
		case <-ctx.Done():
			wg1.Wait()
			if bondManager != nil {
				bondManager.Wait()
			}
			return
		default:
		}
		// Wait for a connection.
		conn, err := listener.Accept()
		if err != nil {
			log.Println(err)
			continue
		}
		lease, rejection := connectionLimiter.Acquire(conn.RemoteAddr())
		if lease == nil {
			metrics.Process.ConnectionLimitRejected()
			count := connectionRejections.Add(1)
			if shouldLogResourceRejection(count) {
				log.Printf("transport connection rejected from %v: %s (total rejections: %d)", conn.RemoteAddr(), rejection, count)
			}
			_ = conn.Close()
			continue
		}
		metrics.Process.TransportConnectionOpened()
		releaseConnection := func() {
			if lease.Release() {
				metrics.Process.TransportConnectionClosed()
			}
		}
		wg1.Add(1)
		go func(conn net.Conn, releaseConnection func()) {
			defer wg1.Done()
			releaseOnReturn := true
			defer func() {
				if releaseOnReturn {
					releaseConnection()
				}
			}()
			ownsConn := true
			defer func() {
				if !ownsConn {
					return
				}
				if closeErr := conn.Close(); closeErr != nil {
					log.Printf("failed to close incoming connection: %s", closeErr)
				}
			}()
			log.Printf("Connection from %s\n", conn.RemoteAddr())

			// Perform the handshake with a 30-second timeout
			ctx1, cancel1 := context.WithTimeout(ctx, 30*time.Second)
			defer cancel1()

			dtlsConn, ok := conn.(*dtls.Conn)
			if !ok {
				log.Println("Type error: expected *dtls.Conn")
				return
			}
			log.Println("Start handshake")
			if err := dtlsConn.HandshakeContext(ctx1); err != nil {
				log.Printf("Handshake failed: %v", err)
				return
			}
			log.Println("Handshake done")
			if clientAuthToken != nil {
				if err := sessionauth.Verify(ctx1, dtlsConn, *clientAuthToken); err != nil {
					metrics.Process.AuthFailed()
					log.Printf("Client authentication failed from %s: %v", conn.RemoteAddr(), err)
					return
				}
				log.Printf("Client authentication succeeded: %s", conn.RemoteAddr())
			}

			if *vlessMode {
				if *vlessBond {
					if err := bondManager.AddWithCleanup(ctx, dtlsConn, releaseConnection); err != nil {
						log.Printf("VLESS bond path rejected: %s", err)
						return
					}
					ownsConn = false
					releaseOnReturn = false
					log.Printf("VLESS bond path accepted: %s\n", conn.RemoteAddr())
					return
				}
				handleVLESSConnection(ctx, dtlsConn, *connect, backendGate)
			} else {
				handleUDPConnection(ctx, conn, *connect)
			}

			log.Printf("Connection closed: %s\n", conn.RemoteAddr())
		}(conn, releaseConnection)
	}
}

func validateServerVLESSFlags(vlessMode, vlessBond bool) error {
	if vlessBond && !vlessMode {
		return fmt.Errorf("-vless-bond requires -vless")
	}
	return nil
}

func validateServerCompatibilityFlags(wrap bool, wrapKey string, generateWrapKey bool) error {
	if wrap || wrapKey != "" || generateWrapKey {
		return fmt.Errorf("WRAP compatibility mode is not implemented in this build")
	}
	return nil
}

func enabledText(enabled bool) string {
	if enabled {
		return "enabled"
	}
	return "disabled"
}

type vlessBondManager struct {
	connectAddr string
	backendGate *backendGate
	mu          sync.Mutex
	groups      map[string]*vlessBondGroup
	wg          sync.WaitGroup
	runGroup    func(context.Context, *vlessBondGroup, func())
}

func newVLESSBondManager(connectAddr string) *vlessBondManager {
	return newVLESSBondManagerWithGate(connectAddr, defaultBackendGate())
}

func newVLESSBondManagerWithGate(connectAddr string, gate *backendGate) *vlessBondManager {
	return &vlessBondManager{
		connectAddr: connectAddr,
		backendGate: gate,
		groups:      make(map[string]*vlessBondGroup),
		runGroup: func(ctx context.Context, group *vlessBondGroup, onDone func()) {
			group.run(ctx, onDone)
		},
	}
}

func (m *vlessBondManager) Add(ctx context.Context, conn net.Conn) error {
	return m.add(ctx, conn, nil)
}

func (m *vlessBondManager) AddWithCleanup(ctx context.Context, conn net.Conn, cleanup func()) error {
	return m.add(ctx, conn, cleanup)
}

func (m *vlessBondManager) add(ctx context.Context, conn net.Conn, cleanup func()) error {
	hello, err := tcputil.ReadBondHelloConfig(conn)
	if err != nil {
		return err
	}
	if err := tcputil.ValidateBondHelloTuning(hello); err != nil {
		return rejectV2BondHello(conn, hello, "PROFILE_MISMATCH", err)
	}
	bondID := hello.BondID
	m.mu.Lock()
	existing := m.groups[bondID]
	if existing != nil && !existing.matchesHello(hello) {
		m.mu.Unlock()
		return rejectV2BondHello(conn, hello, "GROUP_MISMATCH",
			fmt.Errorf("vless bond %s path profile does not match the existing group", existing.shortID()))
	}
	m.mu.Unlock()
	if hello.Version == tcputil.BondProtocolV2 {
		if err := tcputil.WriteBondHelloAck(conn); err != nil {
			return fmt.Errorf("write vless bond V2 acknowledgement: %w", err)
		}
	}

	m.mu.Lock()
	group := m.groups[bondID]
	if group == nil {
		group = &vlessBondGroup{
			id:          bondID,
			connectAddr: m.connectAddr,
			backendGate: m.backendGate,
			pc:          tcputil.NewBondedPacketConn("vless-bond-server:" + bondID),
			hello:       hello,
		}
		m.groups[bondID] = group
	} else if !group.matchesHello(hello) {
		m.mu.Unlock()
		return fmt.Errorf("vless bond %s path profile does not match the existing group", group.shortID())
	}
	m.mu.Unlock()

	// The first path must be visible before run starts, otherwise the server
	// sizes KCP from an empty bond and remains capped at a single-path window.
	if err := group.addWithCleanup(conn, cleanup); err != nil {
		return err
	}
	group.startOnce.Do(func() {
		m.wg.Add(1)
		go func() {
			defer m.wg.Done()
			m.runGroup(ctx, group, func() {
				m.mu.Lock()
				if m.groups[bondID] == group {
					delete(m.groups, bondID)
				}
				m.mu.Unlock()
			})
		}()
	})
	return nil
}

func rejectV2BondHello(conn net.Conn, hello tcputil.BondHello, code string, cause error) error {
	if hello.Version != tcputil.BondProtocolV2 {
		return cause
	}
	if err := tcputil.WriteBondHelloReject(conn, code); err != nil {
		return fmt.Errorf("%w (also failed to send V2 rejection %s: %v)", cause, code, err)
	}
	return cause
}

func (m *vlessBondManager) Wait() {
	m.wg.Wait()
}

type vlessBondGroup struct {
	id          string
	connectAddr string
	backendGate *backendGate
	pc          *tcputil.BondedPacketConn
	hello       tcputil.BondHello
	startOnce   sync.Once
	mu          sync.Mutex
	maxPaths    int
	window      kcpWindowSetter
	appliedWnd  int
}

type kcpWindowSetter interface {
	SetWindowSize(int, int)
}

func (g *vlessBondGroup) matchesHello(hello tcputil.BondHello) bool {
	return g.hello == hello
}

func (g *vlessBondGroup) add(conn net.Conn) error {
	return g.addWithCleanup(conn, nil)
}

func (g *vlessBondGroup) addWithCleanup(conn net.Conn, cleanup func()) error {
	g.mu.Lock()
	if g.hello.Version == tcputil.BondProtocolV2 && g.pc.Count() >= g.hello.ExpectedPaths {
		g.mu.Unlock()
		return fmt.Errorf("vless bond %s already has its negotiated %d paths", g.shortID(), g.hello.ExpectedPaths)
	}
	g.pc.AddConn(conn, cleanup)
	active := g.pc.Count()
	if active > g.maxPaths {
		g.maxPaths = active
	}
	resized, window := g.applyWindowLocked()
	g.mu.Unlock()

	log.Printf("VLESS bond %s: path connected (active: %d)", g.shortID(), active)
	if resized {
		log.Printf("VLESS bond %s: KCP window increased to %d", g.shortID(), window)
	}
	return nil
}

func (g *vlessBondGroup) desiredPathCountLocked() int {
	if g.hello.Version == tcputil.BondProtocolV2 && g.hello.ExpectedPaths > 0 {
		return g.hello.ExpectedPaths
	}
	if g.maxPaths < 1 {
		return 1
	}
	return g.maxPaths
}

func (g *vlessBondGroup) applyWindowLocked() (bool, int) {
	window := tcputil.BondedKCPWindow(g.desiredPathCountLocked())
	if g.window == nil || window <= g.appliedWnd {
		return false, window
	}
	g.window.SetWindowSize(window, window)
	g.appliedWnd = window
	return true, window
}

func (g *vlessBondGroup) installWindowSetter(setter kcpWindowSetter) (int, int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.window = setter
	g.appliedWnd = 0
	pathCount := g.desiredPathCountLocked()
	window := tcputil.BondedKCPWindow(pathCount)
	setter.SetWindowSize(window, window)
	g.appliedWnd = window
	return pathCount, window
}

func (g *vlessBondGroup) clearWindowSetter(setter kcpWindowSetter) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.window == setter {
		g.window = nil
		g.appliedWnd = 0
	}
}

func (g *vlessBondGroup) desiredPathCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.desiredPathCountLocked()
}

func (g *vlessBondGroup) run(ctx context.Context, onDone func()) {
	defer onDone()
	defer func() { _ = g.pc.Close() }()
	closeDone := make(chan struct{})
	stopClose := context.AfterFunc(ctx, func() {
		defer close(closeDone)
		_ = g.pc.Close()
	})
	defer func() {
		if !stopClose() {
			<-closeDone
		}
	}()

	// Scale the receive/send window for the aggregate of bonded paths. Window
	// sizes need not match the peer exactly (unlike FEC), so we size from the
	// paths connected so far (with a floor) — more paths may still join after.
	pathCount := g.desiredPathCount()
	kcpSess, err := tcputil.NewKCPOverPacketConnBonded(g.pc, g.pc.RemoteAddr(), true, pathCount)
	if err != nil {
		log.Printf("VLESS bond %s: KCP session error: %s", g.shortID(), err)
		return
	}
	defer func() {
		if err := kcpSess.Close(); err != nil {
			log.Printf("VLESS bond %s: failed to close KCP session: %v", g.shortID(), err)
		}
	}()
	pathCount, window := g.installWindowSetter(kcpSess)
	defer g.clearWindowSetter(kcpSess)
	log.Printf("KCP session established (vless bond server, id=%s, paths=%d, window=%d)", g.shortID(), pathCount, window)

	smuxSess, err := smux.Server(kcpSess, tcputil.DefaultSmuxConfig())
	if err != nil {
		log.Printf("VLESS bond %s: smux server error: %s", g.shortID(), err)
		return
	}
	defer func() {
		if err := smuxSess.Close(); err != nil {
			log.Printf("VLESS bond %s: failed to close smux session: %v", g.shortID(), err)
		}
	}()
	metrics.Process.SessionOpened()
	defer metrics.Process.SessionClosed()
	log.Printf("smux session established (vless bond server, id=%s)", g.shortID())

	serveSmuxSession(ctx, smuxSess, g.connectAddr, g.backendGate)
}

func (g *vlessBondGroup) shortID() string {
	if len(g.id) <= 8 {
		return g.id
	}
	return g.id[:8]
}

// handleUDPConnection forwards DTLS packets to a UDP backend (WireGuard).
func handleUDPConnection(ctx context.Context, conn net.Conn, connectAddr string) {
	serverConn, err := net.Dial("udp", connectAddr)
	if err != nil {
		log.Println(err)
		return
	}
	defer func() {
		if err = serverConn.Close(); err != nil {
			log.Printf("failed to close outgoing connection: %s", err)
		}
	}()

	var wg sync.WaitGroup
	wg.Add(2)
	ctx2, cancel2 := context.WithCancel(ctx)
	deadlineDone := make(chan struct{})
	stopDeadline := context.AfterFunc(ctx2, func() {
		defer close(deadlineDone)
		if err := conn.SetDeadline(time.Now()); err != nil {
			log.Printf("failed to set incoming deadline: %s", err)
		}
		if err := serverConn.SetDeadline(time.Now()); err != nil {
			log.Printf("failed to set outgoing deadline: %s", err)
		}
	})
	activity := newUDPActivity()
	watchdogDone := make(chan struct{})
	go func() {
		defer close(watchdogDone)
		if activity.waitUntilIdle(ctx2, udpIdleTimeout) {
			log.Printf("UDP session idle for %s; closing", udpIdleTimeout)
			cancel2()
		}
	}()
	go func() {
		defer wg.Done()
		defer cancel2()
		buf := make([]byte, udpPacketBufferSize)
		for {
			n, err1 := conn.Read(buf)
			if err1 != nil {
				if ctx2.Err() == nil {
					log.Printf("read DTLS packet: %s", err1)
				}
				return
			}
			activity.touch()
			_, err1 = serverConn.Write(buf[:n])
			if err1 != nil {
				if ctx2.Err() == nil {
					log.Printf("write backend UDP packet: %s", err1)
				}
				return
			}
		}
	}()
	go func() {
		defer wg.Done()
		defer cancel2()
		buf := make([]byte, udpPacketBufferSize)
		for {
			n, err1 := serverConn.Read(buf)
			if err1 != nil {
				if ctx2.Err() == nil {
					log.Printf("read backend UDP packet: %s", err1)
				}
				return
			}
			_, err1 = conn.Write(buf[:n])
			if err1 != nil {
				if ctx2.Err() == nil {
					log.Printf("write DTLS packet: %s", err1)
				}
				return
			}
		}
	}()
	wg.Wait()
	cancel2()
	<-watchdogDone
	if !stopDeadline() {
		<-deadlineDone
	}
	_ = conn.SetDeadline(time.Time{})
	_ = serverConn.SetDeadline(time.Time{})
}

// handleVLESSConnection creates a KCP+smux session over DTLS and forwards
// each smux stream as a TCP connection to the backend (Xray/VLESS).
func handleVLESSConnection(ctx context.Context, dtlsConn net.Conn, connectAddr string, gate *backendGate) {
	closeDone := make(chan struct{})
	stopClose := context.AfterFunc(ctx, func() {
		defer close(closeDone)
		_ = dtlsConn.Close()
	})
	defer func() {
		if !stopClose() {
			<-closeDone
		}
	}()

	// 1. Create KCP session over DTLS
	kcpSess, err := tcputil.NewKCPOverDTLS(dtlsConn, true)
	if err != nil {
		log.Printf("KCP session error: %s", err)
		return
	}
	defer func() {
		if err := kcpSess.Close(); err != nil {
			log.Printf("failed to close KCP session: %v", err)
		}
	}()
	log.Printf("KCP session established (server)")

	// 2. Create smux server session over KCP
	smuxSess, err := smux.Server(kcpSess, tcputil.DefaultSmuxConfig())
	if err != nil {
		log.Printf("smux server error: %s", err)
		return
	}
	defer func() {
		if err := smuxSess.Close(); err != nil {
			log.Printf("failed to close smux session: %v", err)
		}
	}()
	metrics.Process.SessionOpened()
	defer metrics.Process.SessionClosed()
	log.Printf("smux session established (server)")

	serveSmuxSession(ctx, smuxSess, connectAddr, gate)
}

func serveSmuxSession(ctx context.Context, smuxSess *smux.Session, connectAddr string, gate *backendGate) {
	if gate == nil {
		log.Printf("backend limits are unavailable; closing smux session")
		_ = smuxSess.Close()
		return
	}
	sessionLimiter := gate.NewSessionLimiter()
	sessionCtx, cancelSession := context.WithCancel(ctx)
	closeDone := make(chan struct{})
	stopClose := context.AfterFunc(sessionCtx, func() {
		defer close(closeDone)
		if err := smuxSess.Close(); err != nil && err != smux.ErrGoAway {
			log.Printf("failed to interrupt smux session: %v", err)
		}
	})
	defer func() {
		cancelSession()
		if !stopClose() {
			<-closeDone
		}
	}()

	var wg sync.WaitGroup
	for {
		stream, err := smuxSess.AcceptStream()
		if err != nil {
			select {
			case <-sessionCtx.Done():
			default:
				log.Printf("smux accept error: %s", err)
			}
			break
		}
		lease, rejection, rejectionCount := gate.Acquire(sessionLimiter)
		if lease == nil {
			if shouldLogResourceRejection(rejectionCount) {
				log.Printf("smux stream rejected: %s (total backend rejections: %d)", rejection, rejectionCount)
			}
			_ = stream.Close()
			continue
		}

		wg.Add(1)
		go func(s *smux.Stream, lease *backendLease) {
			defer wg.Done()
			defer lease.Release()

			defer func() {
				if err := s.Close(); err != nil && err != smux.ErrGoAway {
					log.Printf("failed to close smux stream: %v", err)
				}
			}()

			// Connect to backend (Xray/VLESS)
			backendConn, err := (&net.Dialer{Timeout: 10 * time.Second}).DialContext(sessionCtx, "tcp", connectAddr)
			if err != nil {
				if sessionCtx.Err() == nil {
					log.Printf("backend dial error: %s", err)
				}
				return
			}
			defer func() {
				if err := backendConn.Close(); err != nil {
					log.Printf("failed to close backend connection: %v", err)
				}
			}()

			// Bidirectional copy
			pipeConn(sessionCtx, s, backendConn)
		}(stream, lease)
	}
	cancelSession()
	wg.Wait()
}

// pipeConn copies data bidirectionally between two connections.
func pipeConn(ctx context.Context, c1, c2 net.Conn) {
	if err := tcputil.Pipe(ctx, c1, c2); err != nil && ctx.Err() == nil {
		log.Printf("pipeConn: %v", err)
	}
}

package main

import (
	"context"
	cryptorand "crypto/rand"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/cacggghp/vk-turn-proxy/tcputil"
	"github.com/pion/dtls/v3"
	"github.com/pion/dtls/v3/pkg/crypto/selfsign"
	"github.com/xtaci/smux"
)

func main() {
	listen := flag.String("listen", "0.0.0.0:56000", "listen on ip:port")
	connect := flag.String("connect", "", "connect to ip:port")
	vlessMode := flag.Bool("vless", false, "VLESS mode: forward TCP connections (for VLESS) instead of UDP packets")
	vlessBond := flag.Bool("vless-bond", false, "VLESS bond mode: packet-level multipath across TURN/DTLS streams; requires -vless")
	wrap := flag.Bool("wrap", false, "accept WRAP compatibility mode")
	wrapKey := flag.String("wrap-key", "", "64-hex WRAP key")
	genWrapKey := flag.Bool("gen-wrap-key", false, "generate a 64-hex WRAP key and exit")
	tcputil.RegisterTuningFlags()
	flag.Parse()
	if *genWrapKey {
		key, keyErr := generateWrapKey()
		if keyErr != nil {
			log.Fatalf("generate wrap key: %s", keyErr)
		}
		fmt.Println(key)
		return
	}
	if err := validateServerVLESSFlags(*vlessMode, *vlessBond); err != nil {
		log.Fatalf("%s", err)
	}
	if err := validateServerCompatibilityFlags(*wrap, *wrapKey); err != nil {
		log.Fatalf("%s", err)
	}
	log.Printf("vless mode: %s", enabledText(*vlessMode))
	if *vlessMode {
		log.Printf("vless bond: %s", enabledText(*vlessBond))
		if *vlessBond {
			log.Printf("vless bond semantics: packet-level multipath over TURN/DTLS paths")
		}
	}
	if *wrap {
		log.Printf("wrap mode: requested; compatibility flags accepted, packet wrapping is not implemented in this build")
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

	addr, err := net.ResolveUDPAddr("udp", *listen)
	if err != nil {
		panic(err)
	}
	if len(*connect) == 0 {
		log.Panicf("server address is required")
	}
	// Generate a certificate and private key to secure the connection
	certificate, genErr := selfsign.GenerateSelfSigned()
	if genErr != nil {
		panic(genErr)
	}

	//
	// Everything below is the pion-DTLS API! Thanks for using it ❤️.
	//

	// Connect to a DTLS server
	listener, err := dtls.ListenWithOptions(
		"udp",
		addr,
		dtls.WithCertificates(certificate),
		dtls.WithExtendedMasterSecret(dtls.RequireExtendedMasterSecret),
		dtls.WithCipherSuites(dtls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256),
		dtls.WithConnectionIDGenerator(dtls.RandomCIDGenerator(8)),
	)
	if err != nil {
		panic(err)
	}
	context.AfterFunc(ctx, func() {
		if err = listener.Close(); err != nil {
			panic(err)
		}
	})

	fmt.Println("Listening")
	var bondManager *vlessBondManager
	if *vlessMode && *vlessBond {
		bondManager = newVLESSBondManager(*connect)
	}

	wg1 := sync.WaitGroup{}
	for {
		select {
		case <-ctx.Done():
			wg1.Wait()
			return
		default:
		}
		// Wait for a connection.
		conn, err := listener.Accept()
		if err != nil {
			log.Println(err)
			continue
		}
		wg1.Add(1)
		go func(conn net.Conn) {
			defer wg1.Done()
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

			if *vlessMode {
				if *vlessBond {
					if err := bondManager.Add(ctx, dtlsConn); err != nil {
						log.Printf("VLESS bond path rejected: %s", err)
						return
					}
					ownsConn = false
					log.Printf("VLESS bond path accepted: %s\n", conn.RemoteAddr())
					return
				}
				handleVLESSConnection(ctx, dtlsConn, *connect)
			} else {
				handleUDPConnection(ctx, conn, *connect)
			}

			log.Printf("Connection closed: %s\n", conn.RemoteAddr())
		}(conn)
	}
}

func validateServerVLESSFlags(vlessMode, vlessBond bool) error {
	if vlessBond && !vlessMode {
		return fmt.Errorf("-vless-bond requires -vless")
	}
	return nil
}

func validateServerCompatibilityFlags(wrap bool, wrapKey string) error {
	if wrap && !isHexKey64(wrapKey) {
		return fmt.Errorf("bad -wrap-key (need 64 hex)")
	}
	return nil
}

func isHexKey64(value string) bool {
	if len(value) != 64 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func generateWrapKey() (string, error) {
	var key [32]byte
	if _, err := cryptorand.Read(key[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(key[:]), nil
}

func enabledText(enabled bool) string {
	if enabled {
		return "enabled"
	}
	return "disabled"
}

type vlessBondManager struct {
	connectAddr string
	mu          sync.Mutex
	groups      map[string]*vlessBondGroup
}

func newVLESSBondManager(connectAddr string) *vlessBondManager {
	return &vlessBondManager{
		connectAddr: connectAddr,
		groups:      make(map[string]*vlessBondGroup),
	}
}

func (m *vlessBondManager) Add(ctx context.Context, conn net.Conn) error {
	bondID, err := tcputil.ReadBondHello(conn)
	if err != nil {
		return err
	}

	m.mu.Lock()
	group := m.groups[bondID]
	if group == nil {
		group = &vlessBondGroup{
			id:          bondID,
			connectAddr: m.connectAddr,
			pc:          tcputil.NewBondedPacketConn("vless-bond-server:" + bondID),
		}
		m.groups[bondID] = group
		go group.run(ctx, func() {
			m.mu.Lock()
			delete(m.groups, bondID)
			m.mu.Unlock()
		})
	}
	m.mu.Unlock()

	group.add(conn)
	return nil
}

type vlessBondGroup struct {
	id          string
	connectAddr string
	pc          *tcputil.BondedPacketConn
}

func (g *vlessBondGroup) add(conn net.Conn) {
	g.pc.AddConn(conn, nil)
	log.Printf("VLESS bond %s: path connected (active: %d)", g.shortID(), g.pc.Count())
}

func (g *vlessBondGroup) run(ctx context.Context, onDone func()) {
	defer onDone()
	defer func() { _ = g.pc.Close() }()

	kcpSess, err := tcputil.NewKCPOverPacketConn(g.pc, g.pc.RemoteAddr(), true)
	if err != nil {
		log.Printf("VLESS bond %s: KCP session error: %s", g.shortID(), err)
		return
	}
	defer func() {
		if err := kcpSess.Close(); err != nil {
			log.Printf("VLESS bond %s: failed to close KCP session: %v", g.shortID(), err)
		}
	}()
	log.Printf("KCP session established (vless bond server, id=%s)", g.shortID())

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
	log.Printf("smux session established (vless bond server, id=%s)", g.shortID())

	serveSmuxSession(ctx, smuxSess, g.connectAddr)
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
	context.AfterFunc(ctx2, func() {
		if err := conn.SetDeadline(time.Now()); err != nil {
			log.Printf("failed to set incoming deadline: %s", err)
		}
		if err := serverConn.SetDeadline(time.Now()); err != nil {
			log.Printf("failed to set outgoing deadline: %s", err)
		}
	})
	go func() {
		defer wg.Done()
		defer cancel2()
		buf := make([]byte, 1600)
		for {
			select {
			case <-ctx2.Done():
				return
			default:
			}
			if err1 := conn.SetReadDeadline(time.Now().Add(time.Minute * 30)); err1 != nil {
				log.Printf("Failed: %s", err1)
				return
			}
			n, err1 := conn.Read(buf)
			if err1 != nil {
				log.Printf("Failed: %s", err1)
				return
			}

			if err1 = serverConn.SetWriteDeadline(time.Now().Add(time.Minute * 30)); err1 != nil {
				log.Printf("Failed: %s", err1)
				return
			}
			_, err1 = serverConn.Write(buf[:n])
			if err1 != nil {
				log.Printf("Failed: %s", err1)
				return
			}
		}
	}()
	go func() {
		defer wg.Done()
		defer cancel2()
		buf := make([]byte, 1600)
		for {
			select {
			case <-ctx2.Done():
				return
			default:
			}
			if err1 := serverConn.SetReadDeadline(time.Now().Add(time.Minute * 30)); err1 != nil {
				log.Printf("Failed: %s", err1)
				return
			}
			n, err1 := serverConn.Read(buf)
			if err1 != nil {
				log.Printf("Failed: %s", err1)
				return
			}

			if err1 = conn.SetWriteDeadline(time.Now().Add(time.Minute * 30)); err1 != nil {
				log.Printf("Failed: %s", err1)
				return
			}
			_, err1 = conn.Write(buf[:n])
			if err1 != nil {
				log.Printf("Failed: %s", err1)
				return
			}
		}
	}()
	wg.Wait()
}

// handleVLESSConnection creates a KCP+smux session over DTLS and forwards
// each smux stream as a TCP connection to the backend (Xray/VLESS).
func handleVLESSConnection(ctx context.Context, dtlsConn net.Conn, connectAddr string) {
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
	log.Printf("smux session established (server)")

	serveSmuxSession(ctx, smuxSess, connectAddr)
}

func serveSmuxSession(ctx context.Context, smuxSess *smux.Session, connectAddr string) {
	var wg sync.WaitGroup
	for {
		stream, err := smuxSess.AcceptStream()
		if err != nil {
			select {
			case <-ctx.Done():
			default:
				log.Printf("smux accept error: %s", err)
			}
			break
		}

		wg.Add(1)
		go func(s *smux.Stream) {
			defer wg.Done()

			defer func() {
				if err := s.Close(); err != nil && err != smux.ErrGoAway {
					log.Printf("failed to close smux stream: %v", err)
				}
			}()

			// Connect to backend (Xray/VLESS)
			backendConn, err := net.DialTimeout("tcp", connectAddr, 10*time.Second)
			if err != nil {
				log.Printf("backend dial error: %s", err)
				return
			}
			defer func() {
				if err := backendConn.Close(); err != nil {
					log.Printf("failed to close backend connection: %v", err)
				}
			}()

			// Bidirectional copy
			pipeConn(ctx, s, backendConn)
		}(stream)
	}
	wg.Wait()
}

// pipeConn copies data bidirectionally between two connections.
func pipeConn(ctx context.Context, c1, c2 net.Conn) {
	ctx2, cancel := context.WithCancel(ctx)
	defer cancel()

	context.AfterFunc(ctx2, func() {
		if err := c1.SetDeadline(time.Now()); err != nil {
			log.Printf("pipeConn: failed to set deadline c1: %v", err)
		}
		if err := c2.SetDeadline(time.Now()); err != nil {
			log.Printf("pipeConn: failed to set deadline c2: %v", err)
		}
	})

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		if _, err := io.Copy(c1, c2); err != nil {
			log.Printf("pipeConn: c1<-c2 copy error: %v", err)
		}
	}()

	go func() {
		defer wg.Done()
		if _, err := io.Copy(c2, c1); err != nil {
			log.Printf("pipeConn: c2<-c1 copy error: %v", err)
		}
	}()

	wg.Wait()

	// Reset deadlines
	_ = c1.SetDeadline(time.Time{})
	_ = c2.SetDeadline(time.Time{})
}

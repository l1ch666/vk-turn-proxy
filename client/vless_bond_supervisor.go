package main

import (
	"context"
	"fmt"
	"log"
	"math/rand"
	"net"
	"sync"
	"time"

	"github.com/cacggghp/vk-turn-proxy/tcputil"
	"github.com/xtaci/kcp-go/v5"
	"github.com/xtaci/smux"
)

const vlessBondStableGeneration = time.Minute

type bondSmuxSession interface {
	OpenStream() (*smux.Stream, error)
	IsClosed() bool
	CloseChan() <-chan struct{}
}

type vlessBondGeneration struct {
	id        string
	session   bondSmuxSession
	bonded    *tcputil.BondedPacketConn
	startedAt time.Time
	cleanup   func()

	cleanupOnce sync.Once
	failOnce    sync.Once
	failed      chan struct{}
	failureMu   sync.Mutex
	failureErr  error
}

func newVLESSBondGeneration(id string, session bondSmuxSession, bonded *tcputil.BondedPacketConn, cleanup func()) *vlessBondGeneration {
	return &vlessBondGeneration{
		id:        id,
		session:   session,
		bonded:    bonded,
		startedAt: time.Now(),
		cleanup:   cleanup,
		failed:    make(chan struct{}),
	}
}

func (g *vlessBondGeneration) fail(err error) {
	if err == nil {
		err = fmt.Errorf("vless bond generation failed")
	}
	g.failOnce.Do(func() {
		g.failureMu.Lock()
		g.failureErr = err
		g.failureMu.Unlock()
		close(g.failed)
	})
}

func (g *vlessBondGeneration) failure() error {
	g.failureMu.Lock()
	defer g.failureMu.Unlock()
	return g.failureErr
}

func (g *vlessBondGeneration) wait(ctx context.Context) error {
	if g.session == nil {
		return fmt.Errorf("vless bond generation %s has no smux session", shortBondID(g.id))
	}
	var stateChanged <-chan struct{}
	if g.bonded != nil {
		stateChanged = g.bonded.StateChanged()
	}
	for {
		if g.bonded != nil && g.bonded.Count() == 0 {
			return fmt.Errorf("vless bond generation %s has no active paths", shortBondID(g.id))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-g.session.CloseChan():
			return fmt.Errorf("vless bond generation %s smux session closed", shortBondID(g.id))
		case <-g.failed:
			if err := g.failure(); err != nil {
				return err
			}
			return fmt.Errorf("vless bond generation %s failed", shortBondID(g.id))
		case <-stateChanged:
		}
	}
}

func (g *vlessBondGeneration) close() {
	g.cleanupOnce.Do(func() {
		if g.cleanup != nil {
			g.cleanup()
		}
	})
}

type vlessBondSessionSlot struct {
	mu      sync.RWMutex
	current *vlessBondGeneration
	changed chan struct{}
}

func newVLESSBondSessionSlot() *vlessBondSessionSlot {
	return &vlessBondSessionSlot{changed: make(chan struct{})}
}

func (s *vlessBondSessionSlot) publish(generation *vlessBondGeneration) {
	s.mu.Lock()
	s.current = generation
	s.notifyLocked()
	s.mu.Unlock()
}

func (s *vlessBondSessionSlot) clear(generation *vlessBondGeneration) {
	s.mu.Lock()
	if s.current == generation {
		s.current = nil
		s.notifyLocked()
	}
	s.mu.Unlock()
}

func (s *vlessBondSessionSlot) get() *vlessBondGeneration {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.current
}

func (s *vlessBondSessionSlot) wait(ctx context.Context) *vlessBondGeneration {
	for {
		s.mu.RLock()
		generation := s.current
		changed := s.changed
		s.mu.RUnlock()
		if generation != nil {
			return generation
		}
		select {
		case <-ctx.Done():
			return nil
		case <-changed:
		}
	}
}

func (s *vlessBondSessionSlot) notifyLocked() {
	close(s.changed)
	s.changed = make(chan struct{})
}

type vlessBondGenerationFactory func(context.Context) (*vlessBondGeneration, error)
type vlessBondRetryDelay func(int) time.Duration

func superviseVLESSBond(
	ctx context.Context,
	slot *vlessBondSessionSlot,
	factory vlessBondGenerationFactory,
	retryDelay vlessBondRetryDelay,
) {
	attempt := 0
	for {
		if ctx.Err() != nil {
			return
		}

		generation, err := factory(ctx)
		if err == nil && generation == nil {
			err = fmt.Errorf("VLESS bond generation factory returned nil")
		}
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Printf("VLESS bond generation setup failed: %s", err)
			if !waitContextDelay(ctx, retryDelay(attempt)) {
				return
			}
			attempt++
			continue
		}

		slot.publish(generation)
		err = generation.wait(ctx)
		slot.clear(generation)
		lifetime := time.Since(generation.startedAt)
		generation.close()
		if ctx.Err() != nil {
			return
		}
		log.Printf("VLESS bond generation %s stopped after %s: %s", shortBondID(generation.id), lifetime.Round(time.Millisecond), err)
		if lifetime >= vlessBondStableGeneration {
			attempt = 0
		}
		if !waitContextDelay(ctx, retryDelay(attempt)) {
			return
		}
		attempt++
	}
}

func createVLESSBondGeneration(
	ctx context.Context,
	tp *turnParams,
	peer *net.UDPAddr,
	numSessions int,
) (*vlessBondGeneration, error) {
	bondID, err := generateBondID()
	if err != nil {
		return nil, fmt.Errorf("generate vless bond id: %w", err)
	}
	generationCtx, cancel := context.WithCancel(ctx)
	bonded := tcputil.NewBondedPacketConn("vless-bond-client:" + bondID)
	var (
		pathWG      sync.WaitGroup
		kcpSession  *kcp.UDPSession
		smuxSession *smux.Session
		cleanupOnce sync.Once
	)
	cleanup := func() {
		cleanupOnce.Do(func() {
			cancel()
			if smuxSession != nil {
				_ = smuxSession.Close()
			}
			if kcpSession != nil {
				_ = kcpSession.Close()
			}
			_ = bonded.Close()
			pathWG.Wait()
		})
	}

	for i := 0; i < numSessions; i++ {
		pathWG.Add(1)
		go func(id int) {
			defer pathWG.Done()
			if !waitContextDelay(generationCtx, time.Duration(id)*300*time.Millisecond) {
				return
			}
			maintainVLESSBondPath(generationCtx, tp, peer, id, bondID, bonded)
		}(i)
	}

	log.Printf("VLESS bond generation %s: waiting for first path (total: %d)", shortBondID(bondID), numSessions)
	if err := waitForFirstBondPath(generationCtx, bonded); err != nil {
		cleanup()
		return nil, err
	}

	kcpSession, err = tcputil.NewKCPOverPacketConnBonded(bonded, bonded.RemoteAddr(), false, numSessions)
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("VLESS bond KCP session: %w", err)
	}
	smuxSession, err = smux.Client(kcpSession, tcputil.DefaultSmuxConfig())
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("VLESS bond smux client: %w", err)
	}
	log.Printf("VLESS bond generation %s established (active paths: %d)", shortBondID(bondID), bonded.Count())
	return newVLESSBondGeneration(bondID, smuxSession, bonded, cleanup), nil
}

func waitForFirstBondPath(ctx context.Context, bonded *tcputil.BondedPacketConn) error {
	for {
		if bonded.Count() > 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-bonded.StateChanged():
		}
	}
}

func runSupervisedVLESSBondMode(ctx context.Context, tp *turnParams, peer *net.UDPAddr, listenAddr string, numSessions int) {
	numSessions = normalizeVLESSSessionCount(numSessions)
	listener, err := net.Listen("tcp", listenAddr)
	if err != nil {
		log.Panicf("TCP listen: %s", err)
	}
	defer func() { _ = listener.Close() }()
	context.AfterFunc(ctx, func() { _ = listener.Close() })

	log.Printf("vless mode: enabled")
	log.Printf("vless bond: enabled")
	log.Printf("vless bond semantics: packet-level multipath over %d TURN/DTLS paths", numSessions)

	slot := newVLESSBondSessionSlot()
	var supervisorWG sync.WaitGroup
	supervisorWG.Add(1)
	go func() {
		defer supervisorWG.Done()
		superviseVLESSBond(ctx, slot, func(factoryCtx context.Context) (*vlessBondGeneration, error) {
			return createVLESSBondGeneration(factoryCtx, tp, peer, numSessions)
		}, vlessBondGenerationRetryDelay)
	}()

	if slot.wait(ctx) == nil {
		supervisorWG.Wait()
		return
	}
	log.Printf("VLESS bond: listening on %s (supervised smux session, %d paths)", listenAddr, numSessions)

	var connectionWG sync.WaitGroup
	for {
		tcpConn, acceptErr := listener.Accept()
		if acceptErr != nil {
			if ctx.Err() != nil {
				connectionWG.Wait()
				supervisorWG.Wait()
				return
			}
			log.Printf("TCP accept error: %s", acceptErr)
			continue
		}

		generation := slot.get()
		if generation == nil || generation.session.IsClosed() {
			log.Printf("VLESS bond has no active generation, rejecting connection")
			_ = tcpConn.Close()
			continue
		}

		connectionWG.Add(1)
		go func(tc net.Conn, active *vlessBondGeneration) {
			defer connectionWG.Done()
			defer func() { _ = tc.Close() }()
			stream, openErr := active.session.OpenStream()
			if openErr != nil {
				active.fail(fmt.Errorf("smux open stream: %w", openErr))
				log.Printf("VLESS bond smux open stream error: %s", openErr)
				return
			}
			defer func() { _ = stream.Close() }()
			pipe(ctx, tc, stream)
		}(tcpConn, generation)
	}
}

func vlessBondGenerationRetryDelay(attempt int) time.Duration {
	if attempt < 0 {
		attempt = 0
	}
	delay := time.Second
	for i := 0; i < attempt && delay < 30*time.Second; i++ {
		delay *= 2
		if delay > 30*time.Second {
			delay = 30 * time.Second
		}
	}
	half := delay / 2
	return half + time.Duration(rand.Int63n(int64(delay-half)+1))
}

func vlessBondPathRetryDelay(base time.Duration) time.Duration {
	spread := base / 2
	if spread <= 0 {
		return base
	}
	return base + time.Duration(rand.Int63n(int64(spread)+1))
}

func waitContextDelay(ctx context.Context, delay time.Duration) bool {
	if delay <= 0 {
		return ctx.Err() == nil
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func shortBondID(id string) string {
	if len(id) <= 8 {
		return id
	}
	return id[:8]
}

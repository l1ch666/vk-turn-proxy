package diagnostics

import (
	"context"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/pprof"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cacggghp/vk-turn-proxy/metrics"
)

const diagnosticsShutdownTimeout = 3 * time.Second

// Config is the validated runtime diagnostics configuration.
type Config struct {
	ListenAddress string
	BearerToken   string
	EnablePprof   bool
}

// Validate rejects accidental external exposure and incomplete authentication.
func (c Config) Validate() error {
	listenAddress := strings.TrimSpace(c.ListenAddress)
	if listenAddress == "" {
		if c.EnablePprof || c.BearerToken != "" {
			return fmt.Errorf("diagnostics options require a listen address")
		}
		return nil
	}
	if err := validateLoopbackAddress(listenAddress); err != nil {
		return err
	}
	if _, err := decodeToken(strings.TrimSpace(c.BearerToken)); err != nil {
		return err
	}
	return nil
}

func validateLoopbackAddress(address string) error {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("invalid diagnostics listen address: %w", err)
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("diagnostics listen host must be a literal loopback IP address")
	}
	portNumber, err := strconv.Atoi(port)
	if err != nil || portNumber < 0 || portNumber > 65535 {
		return fmt.Errorf("diagnostics listen port must be in 0..65535")
	}
	return nil
}

// Server owns one loopback HTTP diagnostics listener.
type Server struct {
	httpServer *http.Server
	listener   net.Listener
	done       chan struct{}

	closeOnce sync.Once
	closeErr  error
	errMu     sync.Mutex
	serveErr  error
}

// Start synchronously binds the configured listener and serves diagnostics in
// the background. A disabled configuration returns (nil, nil).
func Start(ctx context.Context, config Config, registry *metrics.Registry) (*Server, error) {
	config.ListenAddress = strings.TrimSpace(config.ListenAddress)
	config.BearerToken = strings.TrimSpace(config.BearerToken)
	if err := config.Validate(); err != nil {
		return nil, err
	}
	if config.ListenAddress == "" {
		return nil, nil
	}
	if ctx == nil {
		return nil, fmt.Errorf("diagnostics context must not be nil")
	}
	if registry == nil {
		return nil, fmt.Errorf("diagnostics metrics registry must not be nil")
	}
	token, _ := decodeToken(config.BearerToken)

	listener, err := net.Listen("tcp", config.ListenAddress)
	if err != nil {
		return nil, fmt.Errorf("listen for diagnostics: %w", err)
	}
	tcpAddress, ok := listener.Addr().(*net.TCPAddr)
	if !ok || tcpAddress.IP == nil || !tcpAddress.IP.IsLoopback() {
		_ = listener.Close()
		return nil, fmt.Errorf("diagnostics listener resolved outside loopback")
	}

	httpServer := &http.Server{
		Handler:           newHandler(registry, token, config.EnablePprof),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		IdleTimeout:       30 * time.Second,
		MaxHeaderBytes:    16 << 10,
	}
	server := &Server{
		httpServer: httpServer,
		listener:   listener,
		done:       make(chan struct{}),
	}
	go server.serve()
	context.AfterFunc(ctx, func() {
		if err := server.Close(); err != nil {
			log.Printf("diagnostics shutdown error: %v", err)
		}
	})
	log.Printf("diagnostics listening on http://%s (pprof: %t)", listener.Addr(), config.EnablePprof)
	return server, nil
}

func (s *Server) serve() {
	err := s.httpServer.Serve(s.listener)
	if errors.Is(err, http.ErrServerClosed) {
		err = nil
	}
	s.errMu.Lock()
	s.serveErr = err
	s.errMu.Unlock()
	if err != nil {
		log.Printf("diagnostics server error: %v", err)
	}
	close(s.done)
}

// Addr returns the bound loopback address, including the selected port.
func (s *Server) Addr() net.Addr {
	if s == nil {
		return nil
	}
	return s.listener.Addr()
}

// Done closes after the HTTP serving loop exits.
func (s *Server) Done() <-chan struct{} {
	if s == nil {
		done := make(chan struct{})
		close(done)
		return done
	}
	return s.done
}

// Err returns an unexpected serving error after Done closes.
func (s *Server) Err() error {
	if s == nil {
		return nil
	}
	s.errMu.Lock()
	defer s.errMu.Unlock()
	return s.serveErr
}

// Close gracefully stops diagnostics, then forcibly closes any handler that
// outlives the bounded shutdown window. It is safe to call repeatedly.
func (s *Server) Close() error {
	if s == nil {
		return nil
	}
	s.closeOnce.Do(func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), diagnosticsShutdownTimeout)
		defer cancel()
		shutdownErr := s.httpServer.Shutdown(shutdownCtx)
		closeErr := s.httpServer.Close()
		if errors.Is(shutdownErr, http.ErrServerClosed) {
			shutdownErr = nil
		}
		if errors.Is(closeErr, http.ErrServerClosed) {
			closeErr = nil
		}
		s.closeErr = errors.Join(shutdownErr, closeErr)
	})
	return s.closeErr
}

func newHandler(registry *metrics.Registry, token [32]byte, enablePprof bool) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", readOnlyJSON(func() any {
		return struct {
			Status string `json:"status"`
		}{Status: "ok"}
	}))
	mux.HandleFunc("/metrics", readOnlyJSON(func() any { return registry.Snapshot() }))
	if enablePprof {
		pprofMux := http.NewServeMux()
		pprofMux.HandleFunc("GET /debug/pprof/", pprof.Index)
		pprofMux.HandleFunc("GET /debug/pprof/cmdline", pprof.Cmdline)
		pprofMux.HandleFunc("GET /debug/pprof/profile", pprof.Profile)
		pprofMux.HandleFunc("GET /debug/pprof/symbol", pprof.Symbol)
		pprofMux.HandleFunc("GET /debug/pprof/trace", pprof.Trace)
		mux.Handle("/debug/pprof/", limitPprofDuration(pprofMux))
	}
	return securityHeaders(requireBearerToken(token, mux))
}

func readOnlyJSON(snapshot func() any) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		if err := json.NewEncoder(w).Encode(snapshot()); err != nil {
			log.Printf("diagnostics JSON response error: %v", err)
		}
	}
}

func requireBearerToken(token [32]byte, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		scheme, rawToken, ok := strings.Cut(strings.TrimSpace(r.Header.Get("Authorization")), " ")
		var supplied [32]byte
		decoded, decodeErr := hex.Decode(supplied[:], []byte(strings.TrimSpace(rawToken)))
		if !ok || !strings.EqualFold(scheme, "Bearer") || decodeErr != nil || decoded != len(supplied) ||
			subtle.ConstantTimeCompare(token[:], supplied[:]) != 1 {
			w.Header().Set("WWW-Authenticate", `Bearer realm="vk-turn-proxy diagnostics"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		next.ServeHTTP(w, r)
	})
}

func limitPprofDuration(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		limit := 60
		if r.URL.Path == "/debug/pprof/trace" {
			limit = 15
		}
		if raw := r.URL.Query().Get("seconds"); raw != "" {
			seconds, err := strconv.Atoi(raw)
			if err != nil || seconds < 1 || seconds > limit {
				http.Error(w, fmt.Sprintf("seconds must be in 1..%d", limit), http.StatusBadRequest)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

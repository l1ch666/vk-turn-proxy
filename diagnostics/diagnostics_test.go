package diagnostics

import (
	"context"
	"encoding/json"
	"flag"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/l1ch666/vk-turn-proxy/v2/metrics"
)

const testToken = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func TestConfigValidatesLoopbackAndToken(t *testing.T) {
	valid := []string{"127.0.0.1:0", "127.42.0.1:6060", "[::1]:0"}
	for _, address := range valid {
		config := Config{ListenAddress: address, BearerToken: testToken}
		if err := config.Validate(); err != nil {
			t.Errorf("Validate(%q) = %v", address, err)
		}
	}

	invalid := []string{"0.0.0.0:6060", "[::]:6060", "192.168.1.2:6060", "localhost:6060", ":6060", "127.0.0.1", "127.0.0.1:http"}
	for _, address := range invalid {
		config := Config{ListenAddress: address, BearerToken: testToken}
		if err := config.Validate(); err == nil {
			t.Errorf("Validate(%q) unexpectedly succeeded", address)
		}
	}

	for _, token := range []string{"", "short", strings.Repeat("z", 64), strings.Repeat("a", 62)} {
		config := Config{ListenAddress: "127.0.0.1:0", BearerToken: token}
		if err := config.Validate(); err == nil {
			t.Errorf("Validate accepted token %q", token)
		}
	}
}

func TestOptionsLoadTokenFromEnvironmentAndFile(t *testing.T) {
	t.Setenv(TokenEnvironmentVariable, testToken)
	options := Options{ListenAddress: "127.0.0.1:0"}
	config, err := options.Config()
	if err != nil {
		t.Fatalf("Config from environment: %v", err)
	}
	if config.BearerToken != testToken {
		t.Fatal("environment token was not loaded")
	}

	fileToken := strings.Repeat("a", 64)
	path := filepath.Join(t.TempDir(), "diagnostics.token")
	if err := os.WriteFile(path, []byte(fileToken+"\n"), 0o600); err != nil {
		t.Fatalf("write token file: %v", err)
	}
	options.TokenFile = path
	config, err = options.Config()
	if err != nil {
		t.Fatalf("Config from file: %v", err)
	}
	if config.BearerToken != fileToken {
		t.Fatal("token file did not take precedence")
	}
}

func TestOptionsRejectInactiveSettingsAndMissingToken(t *testing.T) {
	t.Setenv(TokenEnvironmentVariable, "")
	for _, options := range []Options{
		{EnablePprof: true},
		{TokenFile: "token"},
		{ListenAddress: "127.0.0.1:0"},
	} {
		if _, err := options.Config(); err == nil {
			t.Fatalf("Config(%+v) unexpectedly succeeded", options)
		}
	}
}

func TestRegisterFlags(t *testing.T) {
	fs := flag.NewFlagSet("diagnostics", flag.ContinueOnError)
	options := RegisterFlags(fs)
	if err := fs.Parse([]string{"-diagnostics-listen=127.0.0.1:6060", "-diagnostics-token-file=token", "-diagnostics-pprof"}); err != nil {
		t.Fatalf("parse flags: %v", err)
	}
	if options.ListenAddress != "127.0.0.1:6060" || options.TokenFile != "token" || !options.EnablePprof {
		t.Fatalf("parsed options = %+v", options)
	}
}

func TestServerMetricsAuthenticationAndContextShutdown(t *testing.T) {
	var registry metrics.Registry
	registry.SessionOpened()
	path := registry.OpenPath("test/path-1")
	path.ObserveRead(42, nil)
	defer path.Close()

	ctx, cancel := context.WithCancel(context.Background())
	server, err := Start(ctx, Config{ListenAddress: "127.0.0.1:0", BearerToken: testToken}, &registry)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	baseURL := "http://" + server.Addr().String()
	client := &http.Client{Timeout: 2 * time.Second}

	assertStatus(t, client, http.MethodGet, baseURL+"/metrics", "", http.StatusUnauthorized)
	assertStatus(t, client, http.MethodGet, baseURL+"/metrics?token="+testToken, "", http.StatusUnauthorized)
	assertStatus(t, client, http.MethodGet, baseURL+"/metrics", "Bearer "+strings.Repeat("f", 64), http.StatusUnauthorized)
	assertStatus(t, client, http.MethodGet, baseURL+"/healthz", "Bearer "+testToken, http.StatusOK)
	assertStatus(t, client, http.MethodPost, baseURL+"/metrics", "Bearer "+testToken, http.StatusMethodNotAllowed)
	assertStatus(t, client, http.MethodGet, baseURL+"/debug/pprof/", "Bearer "+testToken, http.StatusNotFound)

	request, _ := http.NewRequest(http.MethodGet, baseURL+"/metrics", nil)
	request.Header.Set("Authorization", "Bearer "+testToken)
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer func() { _ = response.Body.Close() }()
	var snapshot metrics.Snapshot
	if err := json.NewDecoder(response.Body).Decode(&snapshot); err != nil {
		t.Fatalf("decode metrics: %v", err)
	}
	if snapshot.ActiveSessions != 1 || snapshot.ActivePaths != 1 || snapshot.BytesRead != 42 {
		t.Fatalf("metrics snapshot = %+v", snapshot)
	}
	if response.Header.Get("Cache-Control") != "no-store" || response.Header.Get("Access-Control-Allow-Origin") != "" {
		t.Fatalf("unexpected response headers: %v", response.Header)
	}

	cancel()
	select {
	case <-server.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("diagnostics server did not stop after context cancellation")
	}
	if err := server.Err(); err != nil {
		t.Fatalf("serve error: %v", err)
	}
}

func TestServerPprofRequiresAuthenticationAndLimitsDuration(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var registry metrics.Registry
	server, err := Start(ctx, Config{ListenAddress: "127.0.0.1:0", BearerToken: testToken, EnablePprof: true}, &registry)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = server.Close() }()
	baseURL := "http://" + server.Addr().String()
	client := &http.Client{Timeout: 2 * time.Second}

	assertStatus(t, client, http.MethodGet, baseURL+"/debug/pprof/", "", http.StatusUnauthorized)
	assertStatus(t, client, http.MethodGet, baseURL+"/debug/pprof/", "Bearer "+testToken, http.StatusOK)
	assertStatus(t, client, http.MethodPost, baseURL+"/debug/pprof/", "Bearer "+testToken, http.StatusMethodNotAllowed)
	assertStatus(t, client, http.MethodGet, baseURL+"/debug/pprof/profile?seconds=61", "Bearer "+testToken, http.StatusBadRequest)
	assertStatus(t, client, http.MethodGet, baseURL+"/debug/pprof/trace?seconds=16", "Bearer "+testToken, http.StatusBadRequest)
	assertStatus(t, client, http.MethodGet, baseURL+"/debug/pprof/heap?seconds=61", "Bearer "+testToken, http.StatusBadRequest)
}

func TestServerSnapshotsMetricsDuringConcurrentUpdates(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var registry metrics.Registry
	path := registry.OpenPath("concurrent/path-1")
	defer path.Close()
	server, err := Start(ctx, Config{ListenAddress: "127.0.0.1:0", BearerToken: testToken}, &registry)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = server.Close() }()

	stop := make(chan struct{})
	var writers sync.WaitGroup
	writers.Add(1)
	go func() {
		defer writers.Done()
		for {
			select {
			case <-stop:
				return
			default:
				path.ObserveRead(1, nil)
				path.ObserveWrite(time.Time{}, 1, nil)
			}
		}
	}()
	defer func() {
		close(stop)
		writers.Wait()
	}()

	client := &http.Client{Timeout: 2 * time.Second}
	metricsURL := "http://" + server.Addr().String() + "/metrics"
	var previousBytes uint64
	for i := 0; i < 25; i++ {
		request, _ := http.NewRequest(http.MethodGet, metricsURL, nil)
		request.Header.Set("Authorization", "Bearer "+testToken)
		response, err := client.Do(request)
		if err != nil {
			t.Fatalf("GET /metrics: %v", err)
		}
		var snapshot metrics.Snapshot
		decodeErr := json.NewDecoder(response.Body).Decode(&snapshot)
		_ = response.Body.Close()
		if decodeErr != nil {
			t.Fatalf("decode metrics: %v", decodeErr)
		}
		if snapshot.BytesRead < previousBytes {
			t.Fatalf("bytes read regressed from %d to %d", previousBytes, snapshot.BytesRead)
		}
		previousBytes = snapshot.BytesRead
	}
}

func TestStartDisabledAndOccupiedPort(t *testing.T) {
	server, err := Start(context.Background(), Config{}, nil)
	if err != nil || server != nil {
		t.Fatalf("disabled Start = (%v, %v), want (nil, nil)", server, err)
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("occupy port: %v", err)
	}
	defer func() { _ = listener.Close() }()
	var registry metrics.Registry
	if _, err := Start(context.Background(), Config{ListenAddress: listener.Addr().String(), BearerToken: testToken}, &registry); err == nil {
		t.Fatal("Start unexpectedly accepted an occupied port")
	}
}

func assertStatus(t *testing.T, client *http.Client, method, url, authorization string, want int) {
	t.Helper()
	request, err := http.NewRequest(method, url, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if authorization != "" {
		request.Header.Set("Authorization", authorization)
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != want {
		t.Fatalf("%s %s status = %d, want %d", method, url, response.StatusCode, want)
	}
}

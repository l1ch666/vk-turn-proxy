package baseline

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/cacggghp/vk-turn-proxy/metrics"
)

const baselineTestToken = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func TestHTTPMetricsClient(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/metrics" || r.Header.Get("Authorization") != "Bearer "+baselineTestToken {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(metrics.Snapshot{ActiveSessions: 2, BytesRead: 42})
	}))
	defer server.Close()
	client, err := NewHTTPMetricsClient(server.URL, baselineTestToken)
	if err != nil {
		t.Fatalf("NewHTTPMetricsClient: %v", err)
	}
	snapshot, err := client.Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if snapshot.ActiveSessions != 2 || snapshot.BytesRead != 42 {
		t.Fatalf("snapshot = %+v", snapshot)
	}
}

func TestHTTPMetricsClientRejectsUnsafeURLsAndTokens(t *testing.T) {
	for _, rawURL := range []string{
		"https://127.0.0.1:6060",
		"http://localhost:6060",
		"http://192.168.1.2:6060",
		"http://127.0.0.1:6060/metrics",
		"http://user@127.0.0.1:6060",
		"http://127.0.0.1:6060?token=x",
	} {
		if _, err := NewHTTPMetricsClient(rawURL, baselineTestToken); err == nil {
			t.Errorf("unsafe URL accepted: %s", rawURL)
		}
	}
	for _, token := range []string{"", "short", strings.Repeat("z", 64)} {
		if _, err := NewHTTPMetricsClient("http://127.0.0.1:6060", token); err == nil {
			t.Errorf("invalid token accepted: %q", token)
		}
	}
}

func TestHTTPMetricsClientDoesNotFollowRedirects(t *testing.T) {
	var reached atomic.Bool
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		reached.Store(true)
	}))
	defer target.Close()
	redirect := httptest.NewServer(http.RedirectHandler(target.URL, http.StatusFound))
	defer redirect.Close()
	client, err := NewHTTPMetricsClient(redirect.URL, baselineTestToken)
	if err != nil {
		t.Fatalf("NewHTTPMetricsClient: %v", err)
	}
	if _, err := client.Fetch(context.Background()); err == nil {
		t.Fatal("redirected metrics response unexpectedly succeeded")
	}
	if reached.Load() {
		t.Fatal("metrics client followed a redirect and risked forwarding authorization")
	}
}

package baseline

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/l1ch666/vk-turn-proxy/v2/metrics"
)

const maximumMetricsResponseBytes = 2 << 20

type HTTPMetricsClient struct {
	metricsURL string
	token      string
	client     *http.Client
}

func NewHTTPMetricsClient(baseURL, token string) (*HTTPMetricsClient, error) {
	parsed, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil {
		return nil, fmt.Errorf("parse diagnostics URL: %w", err)
	}
	if parsed.Scheme != "http" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" ||
		(parsed.Path != "" && parsed.Path != "/") {
		return nil, fmt.Errorf("diagnostics URL must be an HTTP origin without credentials, path, query, or fragment")
	}
	ip := net.ParseIP(parsed.Hostname())
	if ip == nil || !ip.IsLoopback() {
		return nil, fmt.Errorf("diagnostics URL host must be a literal loopback IP address")
	}
	port, err := strconv.Atoi(parsed.Port())
	if err != nil || port < 1 || port > 65535 {
		return nil, fmt.Errorf("diagnostics URL must contain a port in 1..65535")
	}
	token = strings.TrimSpace(token)
	decoded, err := hex.DecodeString(token)
	if err != nil || len(decoded) != 32 {
		return nil, fmt.Errorf("diagnostics token must contain exactly 64 hexadecimal characters")
	}
	parsed.Path = "/metrics"
	defaultTransport, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return nil, fmt.Errorf("default HTTP transport has an unexpected type")
	}
	transport := defaultTransport.Clone()
	transport.Proxy = nil
	return &HTTPMetricsClient{
		metricsURL: parsed.String(),
		token:      token,
		client: &http.Client{
			Transport: transport,
			Timeout:   5 * time.Second,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}, nil
}

func (c *HTTPMetricsClient) Fetch(ctx context.Context) (metrics.Snapshot, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, c.metricsURL, nil)
	if err != nil {
		return metrics.Snapshot{}, fmt.Errorf("create diagnostics request: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+c.token)
	request.Header.Set("Accept", "application/json")
	response, err := c.client.Do(request)
	if err != nil {
		return metrics.Snapshot{}, fmt.Errorf("request diagnostics metrics: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	limited := io.LimitReader(response.Body, maximumMetricsResponseBytes+1)
	data, err := io.ReadAll(limited)
	if err != nil {
		return metrics.Snapshot{}, fmt.Errorf("read diagnostics metrics: %w", err)
	}
	if len(data) > maximumMetricsResponseBytes {
		return metrics.Snapshot{}, fmt.Errorf("diagnostics metrics response exceeds %d bytes", maximumMetricsResponseBytes)
	}
	if response.StatusCode != http.StatusOK {
		return metrics.Snapshot{}, fmt.Errorf("diagnostics metrics returned HTTP %d", response.StatusCode)
	}
	var snapshot metrics.Snapshot
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(&snapshot); err != nil {
		return metrics.Snapshot{}, fmt.Errorf("decode diagnostics metrics: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return metrics.Snapshot{}, fmt.Errorf("diagnostics metrics contains trailing JSON")
	}
	return snapshot, nil
}

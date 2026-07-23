package main

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func TestRewriteProxyRedirectLocation(t *testing.T) {
	t.Parallel()

	targetURL, err := url.Parse("https://id.vk.ru/captcha")
	if err != nil {
		t.Fatalf("failed to parse target URL: %v", err)
	}
	endpoint := &captchaEndpoint{
		host:    "127.0.0.1:54321",
		origin:  "http://127.0.0.1:54321",
		prefix:  "/0123456789abcdef",
		baseURL: "http://127.0.0.1:54321/0123456789abcdef",
	}

	testCases := []struct {
		name     string
		location string
		want     string
		ok       bool
	}{
		{
			name:     "keeps safe relative path",
			location: "/captcha?step=2",
			want:     "http://127.0.0.1:54321/0123456789abcdef/captcha?step=2",
			ok:       true,
		},
		{
			name:     "rewrites same-origin absolute URL",
			location: "https://id.vk.ru/captcha?step=2",
			want:     "http://127.0.0.1:54321/0123456789abcdef/captcha?step=2",
			ok:       true,
		},
		{
			name:     "blocks scheme-relative redirect",
			location: "//evil.example/captcha",
			ok:       false,
		},
		{
			name:     "blocks slash-backslash redirect",
			location: `/\evil.example/captcha`,
			ok:       false,
		},
		{
			name:     "blocks lookalike absolute host",
			location: "https://id.vk.ru.evil.example/captcha",
			ok:       false,
		},
	}

	for _, tc := range testCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, ok := rewriteProxyRedirectLocation(tc.location, targetURL, endpoint)
			if ok != tc.ok {
				t.Fatalf("rewriteProxyRedirectLocation() ok = %v, want %v", ok, tc.ok)
			}
			if got != tc.want {
				t.Fatalf("rewriteProxyRedirectLocation() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestCaptchaEndpointUsesEphemeralLoopbackPortAndNonce(t *testing.T) {
	first, err := newCaptchaEndpoint()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = first.listener.Close() })
	second, err := newCaptchaEndpoint()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = second.listener.Close() })

	if first.host == second.host {
		t.Fatalf("parallel endpoints unexpectedly share %s", first.host)
	}
	if first.prefix == second.prefix || len(strings.TrimPrefix(first.prefix, "/")) != 32 {
		t.Fatalf("endpoint nonces are invalid or reused: %q / %q", first.prefix, second.prefix)
	}
	parsed, err := url.Parse(first.baseURL)
	if err != nil || !isLoopbackHTTPURL(parsed) {
		t.Fatalf("endpoint URL %q is not a loopback HTTP URL: %v", first.baseURL, err)
	}
}

func TestWaitForCaptchaResultReturnsKey(t *testing.T) {
	keyCh := make(chan string, 1)
	keyCh <- "solved"

	got, err := waitForCaptchaResult(context.Background(), keyCh)
	if err != nil {
		t.Fatalf("waitForCaptchaResult returned error: %v", err)
	}
	if got != "solved" {
		t.Fatalf("waitForCaptchaResult = %q, want %q", got, "solved")
	}
}

func TestWaitForCaptchaResultStopsOnContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := waitForCaptchaResult(ctx, make(chan string))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("waitForCaptchaResult error = %v, want context.Canceled", err)
	}
}

func TestAllowedCaptchaTarget(t *testing.T) {
	t.Parallel()
	tests := map[string]bool{
		"https://id.vk.ru/captcha":                 true,
		"https://static.vk-cdn.net/file.js":        true,
		"https://id.vk.ru.evil.example/captcha":    false,
		"http://id.vk.ru/captcha":                  false,
		"https://127.0.0.1/captcha":                false,
		"https://169.254.169.254/latest/meta-data": false,
		"https://example.com/":                     false,
		"https://user@id.vk.ru/captcha":            false,
	}
	for raw, want := range tests {
		raw, want := raw, want
		t.Run(raw, func(t *testing.T) {
			t.Parallel()
			parsed, err := url.Parse(raw)
			if err != nil {
				t.Fatalf("parse target: %v", err)
			}
			if got := isAllowedCaptchaTarget(parsed); got != want {
				t.Fatalf("isAllowedCaptchaTarget(%q) = %v, want %v", raw, got, want)
			}
		})
	}
}

func TestWindowsBrowserCommandDoesNotUseShell(t *testing.T) {
	commands := browserOpenCommands("windows", "http://127.0.0.1:54321/nonce/captcha")
	if len(commands) != 1 {
		t.Fatalf("windows commands = %d, want 1", len(commands))
	}
	if commands[0].name == "cmd" || commands[0].name == "powershell" {
		t.Fatalf("windows browser opener uses a command shell: %#v", commands[0])
	}
}

func TestRewriteGenericProxyRequestStripsCrossOriginSecrets(t *testing.T) {
	endpoint := &captchaEndpoint{
		host:    "127.0.0.1:54321",
		origin:  "http://127.0.0.1:54321",
		prefix:  "/nonce",
		baseURL: "http://127.0.0.1:54321/nonce",
	}
	primary, _ := url.Parse("https://id.vk.ru/captcha")
	cdn, _ := url.Parse("https://static.userapi.com/app.js")
	request, err := http.NewRequest(http.MethodGet, endpoint.urlForPath("/generic_proxy"), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, header := range []string{"Authorization", "Cookie", "Origin", "Proxy-Authorization", "Referer"} {
		request.Header.Set(header, "secret")
	}

	rewriteGenericProxyRequest(request, cdn, primary, endpoint)
	for _, header := range []string{"Authorization", "Cookie", "Origin", "Proxy-Authorization", "Referer"} {
		if got := request.Header.Get(header); got != "" {
			t.Fatalf("cross-origin %s header leaked as %q", header, got)
		}
	}
	if request.URL.String() != "https://static.userapi.com/app.js" {
		t.Fatalf("rewritten URL = %q", request.URL.String())
	}
}

func TestRewriteProxyCookiesPreservesStrictSameSite(t *testing.T) {
	header := make(http.Header)
	header.Add("Set-Cookie", "session=value; Path=/; Secure; SameSite=Strict")
	rewriteProxyCookies(header)
	cookies := (&http.Response{Header: header}).Cookies()
	if len(cookies) != 1 {
		t.Fatalf("rewritten cookies = %d, want 1", len(cookies))
	}
	if cookies[0].SameSite != http.SameSiteStrictMode {
		t.Fatalf("SameSite = %v, want Strict", cookies[0].SameSite)
	}
	if cookies[0].Secure {
		t.Fatal("loopback cookie remained Secure")
	}
}

func TestGenericProxyMethods(t *testing.T) {
	for _, test := range []struct {
		method     string
		sameOrigin bool
		want       bool
	}{
		{http.MethodGet, false, true},
		{http.MethodHead, false, true},
		{http.MethodPost, false, false},
		{http.MethodPost, true, true},
		{http.MethodPut, true, false},
		{http.MethodDelete, false, false},
	} {
		if got := allowedGenericProxyMethod(test.method, test.sameOrigin); got != test.want {
			t.Errorf("allowedGenericProxyMethod(%q, %v) = %v, want %v", test.method, test.sameOrigin, got, test.want)
		}
	}
}

func TestLocalCaptchaEntryURLDoesNotExposeUpstreamQuery(t *testing.T) {
	target, err := url.Parse(
		"https://id.vk.ru/captcha/start?session_token=secret&captcha_sid=123",
	)
	if err != nil {
		t.Fatalf("parse target: %v", err)
	}
	endpoint := &captchaEndpoint{
		host:    "127.0.0.1:32123",
		origin:  "http://127.0.0.1:32123",
		prefix:  "/0123456789abcdef",
		baseURL: "http://127.0.0.1:32123/0123456789abcdef",
	}

	raw := localCaptchaEntryURLForTarget(target, endpoint)
	if strings.Contains(raw, "secret") || strings.Contains(raw, "captcha_sid") {
		t.Fatalf("local entry URL exposed upstream query: %s", raw)
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse local entry URL: %v", err)
	}
	if parsed.Path != endpoint.prefix+target.Path || parsed.RawQuery != localCaptchaEntryQuery {
		t.Fatalf("local entry URL = %s, want target path plus entry marker", raw)
	}
}

package main

import (
	"context"
	"errors"
	"net/url"
	"testing"
)

func TestRewriteProxyRedirectLocation(t *testing.T) {
	t.Parallel()

	targetURL, err := url.Parse("https://id.vk.ru/captcha")
	if err != nil {
		t.Fatalf("failed to parse target URL: %v", err)
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
			want:     "/captcha?step=2",
			ok:       true,
		},
		{
			name:     "rewrites same-origin absolute URL",
			location: "https://id.vk.ru/captcha?step=2",
			want:     "http://localhost:8765/captcha?step=2",
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

			got, ok := rewriteProxyRedirectLocation(tc.location, targetURL)
			if ok != tc.ok {
				t.Fatalf("rewriteProxyRedirectLocation() ok = %v, want %v", ok, tc.ok)
			}
			if got != tc.want {
				t.Fatalf("rewriteProxyRedirectLocation() = %q, want %q", got, tc.want)
			}
		})
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

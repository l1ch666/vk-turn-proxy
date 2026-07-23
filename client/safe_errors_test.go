package main

import (
	"strings"
	"testing"
)

func TestResponseShapeErrorDoesNotLeakValues(t *testing.T) {
	err := responseShapeError("bad response", map[string]interface{}{
		"credential":    "turn-password",
		"success_token": "captcha-token",
	})
	message := err.Error()
	for _, secret := range []string{"turn-password", "captcha-token"} {
		if strings.Contains(message, secret) {
			t.Fatalf("error leaked %q: %s", secret, message)
		}
	}
	for _, field := range []string{"credential", "success_token"} {
		if !strings.Contains(message, field) {
			t.Fatalf("error omitted response field %q: %s", field, message)
		}
	}
}

func TestCaptchaDebugRedactionRemovesChallengeSecrets(t *testing.T) {
	got := redactSessionToken([]byte(
		"session_token=session-secret&hash=puzzle-hash&answer=puzzle-answer&device=kept",
	))
	for _, secret := range []string{"session-secret", "puzzle-hash", "puzzle-answer"} {
		if strings.Contains(got, secret) {
			t.Fatalf("captcha debug redaction leaked %q: %s", secret, got)
		}
	}
	if !strings.Contains(got, "device=kept") {
		t.Fatalf("captcha debug redaction removed non-secret structure: %s", got)
	}
}

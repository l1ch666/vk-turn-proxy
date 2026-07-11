package main

import (
	"errors"
	"testing"
	"time"
)

func TestCaptchaSolveModeForAttempt(t *testing.T) {
	t.Parallel()

	t.Run("default flow", func(t *testing.T) {
		t.Parallel()

		mode, ok := captchaSolveModeForAttempt(0, false, true)
		if !ok || mode != captchaSolveModeAuto {
			t.Fatalf("expected first attempt to use auto captcha, got mode=%v ok=%v", mode, ok)
		}

		mode, ok = captchaSolveModeForAttempt(1, false, true)
		if !ok || mode != captchaSolveModeSliderPOC {
			t.Fatalf("expected second attempt to use slider POC, got mode=%v ok=%v", mode, ok)
		}

		mode, ok = captchaSolveModeForAttempt(2, false, true)
		if !ok || mode != captchaSolveModeManual {
			t.Fatalf("expected third attempt to use manual captcha, got mode=%v ok=%v", mode, ok)
		}

		if _, ok = captchaSolveModeForAttempt(3, false, true); ok {
			t.Fatal("expected no fourth captcha attempt in default flow")
		}
	})

	t.Run("manual only flow", func(t *testing.T) {
		t.Parallel()

		mode, ok := captchaSolveModeForAttempt(0, true, true)
		if !ok || mode != captchaSolveModeManual {
			t.Fatalf("expected manual mode on first attempt, got mode=%v ok=%v", mode, ok)
		}

		if _, ok = captchaSolveModeForAttempt(1, true, true); ok {
			t.Fatal("expected only one manual captcha attempt when manual mode is forced")
		}
	})

	t.Run("flow without slider poc", func(t *testing.T) {
		t.Parallel()

		mode, ok := captchaSolveModeForAttempt(0, false, false)
		if !ok || mode != captchaSolveModeAuto {
			t.Fatalf("expected auto captcha first, got mode=%v ok=%v", mode, ok)
		}

		mode, ok = captchaSolveModeForAttempt(1, false, false)
		if !ok || mode != captchaSolveModeManual {
			t.Fatalf("expected manual captcha second when slider POC is disabled, got mode=%v ok=%v", mode, ok)
		}

		if _, ok = captchaSolveModeForAttempt(2, false, false); ok {
			t.Fatal("expected only two attempts when slider POC is disabled")
		}
	})
}

func TestParseVkCaptchaErrorAllowsRedirectOnlyCaptcha(t *testing.T) {
	t.Parallel()

	captchaErr := ParseVkCaptchaError(map[string]interface{}{
		"error_code":   float64(14),
		"error_msg":    "Captcha needed",
		"captcha_sid":  "12345",
		"redirect_uri": "https://id.vk.ru/captcha?session_token=session-1",
	})

	if captchaErr == nil {
		t.Fatal("expected redirect/session_token captcha payload to parse")
	}
	if !captchaErr.IsCaptchaError() {
		t.Fatal("expected redirect/session_token captcha payload to be auto-solvable captcha")
	}
	if captchaErr.CaptchaImg != "" {
		t.Fatalf("expected empty captcha image for redirect-only captcha, got %q", captchaErr.CaptchaImg)
	}
}

func TestParseVkCaptchaErrorAllowsImageOnlyCaptcha(t *testing.T) {
	t.Parallel()

	captchaErr := ParseVkCaptchaError(map[string]interface{}{
		"error_code":  float64(14),
		"error_msg":   "Captcha needed",
		"captcha_sid": "67890",
		"captcha_img": "https://api.vk.ru/captcha.php?sid=67890",
	})

	if captchaErr == nil {
		t.Fatal("expected legacy image captcha payload to parse")
	}
	if !captchaErr.IsCaptchaError() {
		t.Fatal("expected image captcha payload to be handled as captcha")
	}
	if captchaErr.RedirectURI != "" || captchaErr.SessionToken != "" {
		t.Fatalf("expected no redirect/session token, got redirect=%q session=%q", captchaErr.RedirectURI, captchaErr.SessionToken)
	}
}

func TestParseVkCaptchaErrorAllowsRedirectCaptchaWithoutSid(t *testing.T) {
	t.Parallel()

	captchaErr := ParseVkCaptchaError(map[string]interface{}{
		"error_code":   float64(14),
		"error_msg":    "Captcha needed",
		"redirect_uri": "https://id.vk.ru/captcha?session_token=session-2",
	})

	if captchaErr == nil {
		t.Fatal("expected redirect/session_token captcha without sid to parse")
	}
	if !captchaErr.IsCaptchaError() {
		t.Fatal("expected redirect/session_token captcha without sid to be auto-solvable captcha")
	}
}

func TestParseVkCaptchaErrorAllowsMissingMessage(t *testing.T) {
	t.Parallel()

	captchaErr := ParseVkCaptchaError(map[string]interface{}{
		"error_code":   float64(14),
		"redirect_uri": "https://id.vk.ru/captcha?session_token=session-3",
	})

	if captchaErr == nil {
		t.Fatal("expected captcha payload without error_msg to parse")
	}
	if captchaErr.ErrorMsg != "" {
		t.Fatalf("expected empty error message, got %q", captchaErr.ErrorMsg)
	}
}

func TestIsAuthErrorIsCaseInsensitive(t *testing.T) {
	for _, err := range []error{
		errors.New("401 Unauthorized"),
		errors.New("authentication failed"),
		errors.New("INVALID CREDENTIAL"),
		errors.New("Stale Nonce"),
	} {
		if !isAuthError(err) {
			t.Fatalf("isAuthError(%q) = false, want true", err)
		}
	}
}

func TestRecordTURNAllocationResultInvalidatesCachedCredentials(t *testing.T) {
	const streamID = 900000
	cacheID := getCacheID(streamID)
	defer func() {
		credentialsStore.mu.Lock()
		delete(credentialsStore.caches, cacheID)
		credentialsStore.mu.Unlock()
	}()

	cache := getStreamCache(streamID)
	cache.mutex.Lock()
	cache.creds = TurnCredentials{
		Username:  "user",
		Password:  "password",
		ExpiresAt: time.Now().Add(time.Hour),
		Link:      "link",
	}
	cache.mutex.Unlock()

	authErr := errors.New("401 unauthorized")
	for i := 0; i < maxCacheErrors; i++ {
		recordTURNAllocationResult(streamID, authErr)
	}

	cache.mutex.RLock()
	defer cache.mutex.RUnlock()
	if cache.creds.Username != "" || cache.creds.Password != "" {
		t.Fatalf("cached credentials were not invalidated: %+v", cache.creds)
	}
}

func TestIsFatalCaptchaError(t *testing.T) {
	if !isFatalCaptchaError(errors.New("get TURN creds: FATAL_CAPTCHA_FAILED_NO_STREAMS")) {
		t.Fatal("expected fatal captcha error to be recognized")
	}
	if isFatalCaptchaError(errors.New("temporary setup failure")) {
		t.Fatal("temporary error was classified as fatal")
	}
}

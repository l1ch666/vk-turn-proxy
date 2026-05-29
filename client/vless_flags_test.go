package main

import (
	"encoding/hex"
	"strings"
	"testing"
)

func TestValidateClientVLESSFlagsRequiresVLESSForBond(t *testing.T) {
	err := validateClientVLESSFlags(false, true, 10)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "-vless-bond requires -vless") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestNormalizeVLESSSessionCountKeepsConfiguredStreams(t *testing.T) {
	if got := normalizeVLESSSessionCount(10); got != 10 {
		t.Fatalf("normalizeVLESSSessionCount(10) = %d, want 10", got)
	}
}

func TestNormalizeVLESSSessionCountDefaultsToOne(t *testing.T) {
	if got := normalizeVLESSSessionCount(0); got != 1 {
		t.Fatalf("normalizeVLESSSessionCount(0) = %d, want 1", got)
	}
}

func TestNormalizeStreamsPerCredentialDefaultsToCoreValue(t *testing.T) {
	if got := normalizeStreamsPerCredential(0); got != defaultStreamsPerCache {
		t.Fatalf("normalizeStreamsPerCredential(0) = %d, want %d", got, defaultStreamsPerCache)
	}
}

func TestNormalizeStreamsPerCredentialKeepsConfiguredValue(t *testing.T) {
	if got := normalizeStreamsPerCredential(4); got != 4 {
		t.Fatalf("normalizeStreamsPerCredential(4) = %d, want 4", got)
	}
}

func TestParseDNSServersAddsDefaultPort(t *testing.T) {
	got, err := parseDNSServers("1.1.1.1,8.8.8.8:5353")
	if err != nil {
		t.Fatalf("parseDNSServers returned error: %v", err)
	}
	want := []string{"1.1.1.1:53", "8.8.8.8:5353"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("parseDNSServers = %v, want %v", got, want)
	}
}

func TestValidateClientCompatibilityFlagsAllowsAndroidFlags(t *testing.T) {
	key := strings.Repeat("a", 64)
	if err := validateClientCompatibilityFlags("udp", true, key); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateClientCompatibilityFlagsRejectsBadDNSMode(t *testing.T) {
	err := validateClientCompatibilityFlags("https", false, "")
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "unsupported -dns") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateClientCompatibilityFlagsRejectsBadWrapKey(t *testing.T) {
	err := validateClientCompatibilityFlags("auto", true, "not-hex")
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "bad -wrap-key") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestGenerateWrapKeyReturnsHex32Bytes(t *testing.T) {
	key, err := generateWrapKey()
	if err != nil {
		t.Fatalf("generateWrapKey returned error: %v", err)
	}
	if len(key) != 64 {
		t.Fatalf("wrap key length = %d, want 64", len(key))
	}
	if _, err := hex.DecodeString(key); err != nil {
		t.Fatalf("wrap key is not hex: %v", err)
	}
}

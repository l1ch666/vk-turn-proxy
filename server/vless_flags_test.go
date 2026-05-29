package main

import (
	"encoding/hex"
	"strings"
	"testing"
)

func TestValidateServerVLESSFlagsRequiresVLESSForBond(t *testing.T) {
	err := validateServerVLESSFlags(false, true)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "-vless-bond requires -vless") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateServerVLESSFlagsAllowsVLESSBond(t *testing.T) {
	if err := validateServerVLESSFlags(true, true); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateServerVLESSFlagsAllowsPlainMode(t *testing.T) {
	if err := validateServerVLESSFlags(false, false); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateServerCompatibilityFlagsAllowsWrapKey(t *testing.T) {
	if err := validateServerCompatibilityFlags(true, strings.Repeat("a", 64)); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateServerCompatibilityFlagsRejectsBadWrapKey(t *testing.T) {
	err := validateServerCompatibilityFlags(true, "bad")
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

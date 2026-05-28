package main

import (
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

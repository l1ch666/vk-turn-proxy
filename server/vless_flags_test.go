package main

import (
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

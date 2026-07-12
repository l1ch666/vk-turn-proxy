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

func TestValidateServerCompatibilityFlagsAllowsDefault(t *testing.T) {
	if err := validateServerCompatibilityFlags(false, "", false); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateServerCompatibilityFlagsRejectsUnimplementedWrapModes(t *testing.T) {
	for _, test := range []struct {
		name            string
		wrap            bool
		wrapKey         string
		generateWrapKey bool
	}{
		{name: "wrap", wrap: true},
		{name: "wrap key", wrapKey: strings.Repeat("a", 64)},
		{name: "generate wrap key", generateWrapKey: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := validateServerCompatibilityFlags(test.wrap, test.wrapKey, test.generateWrapKey)
			if err == nil || !strings.Contains(err.Error(), "WRAP compatibility mode is not implemented") {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

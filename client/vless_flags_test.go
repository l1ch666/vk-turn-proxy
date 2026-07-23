package main

import (
	"strings"
	"testing"

	"github.com/l1ch666/vk-turn-proxy/v2/tcputil"
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

func TestValidateClientVLESSFlagsRejectsNegativeSessionCount(t *testing.T) {
	err := validateClientVLESSFlags(true, false, -1)
	if err == nil || !strings.Contains(err.Error(), "must not be negative") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateClientVLESSFlagsAllowsAutomaticSessionCount(t *testing.T) {
	if err := validateClientVLESSFlags(true, false, 0); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateClientVLESSFlagsRejectsTooManyBondPaths(t *testing.T) {
	err := validateClientVLESSFlags(true, true, tcputil.MaxBondPaths+1)
	if err == nil || !strings.Contains(err.Error(), "must not exceed") {
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

func TestNormalizeUDPPathCountPreventsEndpointRoamingByDefault(t *testing.T) {
	if got := normalizeUDPPathCount(false, 10, false); got != 1 {
		t.Fatalf("safe UDP path count = %d, want 1", got)
	}
	if got := normalizeUDPPathCount(false, 10, true); got != 10 {
		t.Fatalf("explicit legacy UDP path count = %d, want 10", got)
	}
	if got := normalizeUDPPathCount(true, 10, false); got != 10 {
		t.Fatalf("VLESS path count = %d, want 10", got)
	}
}

func TestValidateClientListenAddressFailsClosedOutsideLoopback(t *testing.T) {
	for _, address := range []string{"127.0.0.1:9000", "[::1]:9000"} {
		if err := validateClientListenAddress(address, false); err != nil {
			t.Errorf("loopback address %q rejected: %v", address, err)
		}
	}
	for _, address := range []string{"0.0.0.0:9000", "[::]:9000", "localhost:9000"} {
		if err := validateClientListenAddress(address, false); err == nil {
			t.Errorf("non-literal-loopback address %q accepted", address)
		}
		if err := validateClientListenAddress(address, true); err != nil {
			t.Errorf("explicit unsafe address %q rejected: %v", address, err)
		}
	}
	if err := validateClientListenAddress("missing-port", false); err == nil ||
		!strings.Contains(err.Error(), "missing port") {
		t.Fatalf("malformed address error = %v", err)
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

func TestValidateClientCompatibilityFlagsAllowsSupportedFlags(t *testing.T) {
	if err := validateClientCompatibilityFlags(false, "udp", false, "", false); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateClientCompatibilityFlagsRejectsBadDNSMode(t *testing.T) {
	err := validateClientCompatibilityFlags(false, "https", false, "", false)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "unsupported -dns") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateClientCompatibilityFlagsRejectsUnimplementedModes(t *testing.T) {
	tests := []struct {
		name            string
		noDTLS          bool
		dnsMode         string
		wrap            bool
		wrapKey         string
		generateWrapKey bool
		want            string
	}{
		{name: "no DTLS", noDTLS: true, dnsMode: "auto", want: "-no-dtls is not implemented"},
		{name: "DoH", dnsMode: "doh", want: "-dns=doh is not implemented"},
		{name: "wrap", dnsMode: "auto", wrap: true, want: "WRAP compatibility mode is not implemented"},
		{name: "wrap key", dnsMode: "auto", wrapKey: strings.Repeat("a", 64), want: "WRAP compatibility mode is not implemented"},
		{name: "generate wrap key", dnsMode: "auto", generateWrapKey: true, want: "WRAP compatibility mode is not implemented"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validateClientCompatibilityFlags(test.noDTLS, test.dnsMode, test.wrap, test.wrapKey, test.generateWrapKey)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want substring %q", err, test.want)
			}
		})
	}
}

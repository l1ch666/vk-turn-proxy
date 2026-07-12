package tcputil

import (
	"flag"
	"io"
	"math"
	"strings"
	"testing"
)

func validTuningValues() tuningValues {
	return tuningValues{
		window:       256,
		interval:     20,
		nodelay:      1,
		resend:       2,
		nc:           1,
		mtu:          1200,
		smuxRecv:     4 * 1024 * 1024,
		smuxStream:   1024 * 1024,
		dataShards:   0,
		parityShards: 0,
	}
}

func TestValidateTuningRejectsUnsafeValues(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*tuningValues)
		wantErr string
	}{
		{"window zero", func(v *tuningValues) { v.window = 0 }, "kcp-window"},
		{"window above cap", func(v *tuningValues) { v.window = 8193 }, "kcp-window"},
		{"interval below clamp", func(v *tuningValues) { v.interval = 9 }, "kcp-interval"},
		{"interval above clamp", func(v *tuningValues) { v.interval = 5001 }, "kcp-interval"},
		{"bad nodelay", func(v *tuningValues) { v.nodelay = 2 }, "kcp-nodelay"},
		{"negative resend", func(v *tuningValues) { v.resend = -1 }, "kcp-resend"},
		{"bad congestion control", func(v *tuningValues) { v.nc = 2 }, "kcp-nc"},
		{"mtu below kcp minimum", func(v *tuningValues) { v.mtu = 49 }, "kcp-mtu"},
		{"mtu exceeds crypto buffer", func(v *tuningValues) { v.mtu = 1481 }, "kcp-mtu"},
		{"mtu exceeds fec buffer", func(v *tuningValues) {
			v.dataShards, v.parityShards, v.mtu = 10, 3, 1473
		}, "kcp-mtu"},
		{"fec one-sided", func(v *tuningValues) { v.dataShards = 10 }, "kcp-fec"},
		{"fec too many shards", func(v *tuningValues) { v.dataShards, v.parityShards = 255, 2 }, "kcp-fec"},
		{"receive buffer zero", func(v *tuningValues) { v.smuxRecv = 0 }, "smux-recvbuf"},
		{"stream buffer zero", func(v *tuningValues) { v.smuxStream = 0 }, "smux-streambuf"},
		{"stream exceeds receive", func(v *tuningValues) { v.smuxStream = v.smuxRecv + 1 }, "smux-streambuf"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			v := validTuningValues()
			tc.mutate(&v)
			err := validateTuning(v)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("validateTuning() error = %v, want containing %q", err, tc.wantErr)
			}
		})
	}
}

func TestValidateTuningAcceptsBoundaries(t *testing.T) {
	valid := []tuningValues{
		validTuningValues(),
		{window: 1, interval: 10, nodelay: 0, resend: 0, nc: 0, mtu: 50, smuxRecv: 1, smuxStream: 1},
		{window: 8192, interval: 5000, nodelay: 1, resend: math.MaxInt32, nc: 1, mtu: 1480, smuxRecv: math.MaxInt32, smuxStream: math.MaxInt32},
		{window: 256, interval: 20, nodelay: 1, resend: 2, nc: 1, mtu: 1472, smuxRecv: 1024, smuxStream: 1024, dataShards: 255, parityShards: 1},
	}
	for i, v := range valid {
		if err := validateTuning(v); err != nil {
			t.Errorf("valid case %d rejected: %v", i, err)
		}
	}
}

func TestParseEnvIntPreservesZeroAndWhitespace(t *testing.T) {
	got, err := parseEnvInt(" 0 ")
	if err != nil || got != 0 {
		t.Fatalf("parseEnvInt(0) = (%d, %v), want (0, nil)", got, err)
	}
	if _, err := parseEnvInt("bad"); err == nil {
		t.Fatal("parseEnvInt(bad) unexpectedly succeeded")
	}
}

func TestEnvIntPreservesExplicitZero(t *testing.T) {
	const key = "VK_TURN_TEST_ZERO"
	t.Setenv(key, "0")
	if got := envInt(key, 99); got != 0 {
		t.Fatalf("envInt(%s) = %d, want 0", key, got)
	}
}

func TestInvalidFECFlagDoesNotMutateConfiguration(t *testing.T) {
	oldData, oldParity := kcpDataShards, kcpParityShards
	kcpDataShards, kcpParityShards = 10, 3
	t.Cleanup(func() { kcpDataShards, kcpParityShards = oldData, oldParity })

	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	registerTuningFlags(fs)
	if err := fs.Parse([]string{"-kcp-fec=bad"}); err == nil {
		t.Fatal("invalid -kcp-fec unexpectedly succeeded")
	}
	if kcpDataShards != 10 || kcpParityShards != 3 {
		t.Fatalf("invalid flag mutated FEC to %d:%d", kcpDataShards, kcpParityShards)
	}
}

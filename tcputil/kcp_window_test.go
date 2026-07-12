package tcputil

import "testing"

func TestBondedKCPWindowScalesAndSaturatesWithoutOverflow(t *testing.T) {
	maxInt := int(^uint(0) >> 1)
	tests := []struct {
		name      string
		base      int
		pathCount int
		want      int
	}{
		{"zero paths use one", 256, 0, 256},
		{"negative paths use one", 256, -1, 256},
		{"ten paths scale", 256, 10, 2560},
		{"cap normal multiplication", 1024, 9, maxKCPWindow},
		{"cap huge path count", 256, maxInt, maxKCPWindow},
		{"cap huge base", maxInt, 2, maxKCPWindow},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := bondedKCPWindow(tc.base, tc.pathCount); got != tc.want {
				t.Fatalf("bondedKCPWindow(%d, %d) = %d, want %d", tc.base, tc.pathCount, got, tc.want)
			}
		})
	}
}

package tcputil

import "testing"

func TestParseFEC(t *testing.T) {
	valid := []struct {
		in           string
		wantD, wantP int
	}{
		{"10:3", 10, 3},
		{" 10 : 3 ", 10, 3},
		{"", 0, 0},
		{"0:0", 0, 0},
		{"255:1", 255, 1},
	}
	for _, tc := range valid {
		d, p, err := parseFEC(tc.in)
		if err != nil {
			t.Errorf("parseFEC(%q) returned error: %v", tc.in, err)
			continue
		}
		if d != tc.wantD || p != tc.wantP {
			t.Errorf("parseFEC(%q) = (%d,%d), want (%d,%d)", tc.in, d, p, tc.wantD, tc.wantP)
		}
	}

	invalid := []string{
		"10",
		"10:0",
		"0:3",
		"abc:3",
		"10:x",
		"-1:3",
		"10:3:1",
		"255:2",
	}
	for _, in := range invalid {
		if _, _, err := parseFEC(in); err == nil {
			t.Errorf("parseFEC(%q) unexpectedly succeeded", in)
		}
	}
}

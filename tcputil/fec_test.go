package tcputil

import "testing"

func TestParseFEC(t *testing.T) {
	cases := []struct {
		in           string
		wantD, wantP int
	}{
		{"10:3", 10, 3},
		{" 10 : 3 ", 10, 3},
		{"", 0, 0},
		{"0:0", 0, 0},
		{"10", 0, 0},
		{"10:0", 0, 0},
		{"0:3", 0, 0},
		{"abc:3", 0, 0},
		{"10:x", 0, 0},
		{"-1:3", 0, 0},
		{"10:3:1", 0, 0}, // SplitN(2) -> "10","3:1" -> "3:1" not an int -> off
	}
	for _, c := range cases {
		d, p := parseFEC(c.in)
		if d != c.wantD || p != c.wantP {
			t.Errorf("parseFEC(%q) = (%d,%d), want (%d,%d)", c.in, d, p, c.wantD, c.wantP)
		}
	}
}

package update

import "testing"

func TestNewer(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want bool
	}{
		{"0.2.0", "0.1.0", true}, {"0.1.1", "0.1.0", true}, {"1.0.0", "0.9.9", true},
		{"0.10.0", "0.9.0", true}, // numeric, not lexical
		{"0.1.0", "0.1.0", false}, {"0.1.0", "0.2.0", false},
		{"dev", "0.1.0", false}, {"0.2.0", "dev", false}, {"0.2.0", "0.1.0-dirty", false}, {"", "0.1.0", false},
	} {
		if got := Newer(c.a, c.b); got != c.want {
			t.Errorf("Newer(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

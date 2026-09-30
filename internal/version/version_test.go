package version

import "testing"

func TestParse(t *testing.T) {
	for in, want := range map[string][3]int{
		"1.2.3": {1, 2, 3}, "v0.1.0": {0, 1, 0}, "0.1.0-dev": {0, 1, 0}, "2.10.4+build5": {2, 10, 4}, " 1.0.0 ": {1, 0, 0},
	} {
		got, err := Parse(in)
		if err != nil || got != want {
			t.Errorf("Parse(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "1", "1.2", "1.2.3.4", "a.b.c", "1.2.x", "-1.0.0", "1..3"} {
		if _, err := Parse(bad); err == nil {
			t.Errorf("Parse(%q) should fail", bad)
		}
	}
}

func TestLess(t *testing.T) {
	tests := []struct {
		a, b string
		want bool
	}{
		{"0.1.0", "0.2.0", true}, {"0.2.0", "0.1.0", false}, {"0.1.0", "0.1.0", false},
		{"1.0.0", "0.9.9", false}, {"0.9.9", "1.0.0", true}, {"0.1.9", "0.1.10", true}, // numeric, not lexical
		{"0.1.0-dev", "0.1.0", false}, // a prerelease of the minimum satisfies it
	}
	for _, tc := range tests {
		got, err := Less(tc.a, tc.b)
		if err != nil || got != tc.want {
			t.Errorf("Less(%q, %q) = %v, %v; want %v", tc.a, tc.b, got, err, tc.want)
		}
	}
	if _, err := Less("x", "0.1.0"); err == nil {
		t.Error("an unparsable version must error")
	}
}

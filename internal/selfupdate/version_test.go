package selfupdate

import "testing"

func TestCompareVersions(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"0.1.10", "0.1.9", 1},
		{"0.1.9", "0.1.10", -1},
		{"v0.1.5", "0.1.5", 0},
		{"0.2.0", "0.1.999", 1},
		{"1.0.0", "0.9.9", 1},
		// A checkout build sorts below the release of the same count.
		{"0.1.5-dev+abc123", "0.1.5", -1},
		{"0.1.5", "0.1.4-dev+abc123.dirty", 1},
		{"0.1.6-dev+abc", "0.1.5", 1},
		// Build metadata never affects precedence.
		{"0.1.5-dev+aaa", "0.1.5-dev+bbb", 0},
		{"0.1.5+meta", "0.1.5", 0},
		// Semver prerelease rules.
		{"1.0.0-alpha", "1.0.0-alpha.1", -1},
		{"1.0.0-alpha.1", "1.0.0-alpha.beta", -1},
		{"1.0.0-beta.2", "1.0.0-beta.11", -1},
		{"1.0.0-rc.1", "1.0.0", -1},
		// Garbage sorts below everything parseable.
		{"garbage", "0.0.0-dev", -1},
		{"0.0.1", "", 1},
		{"x", "y", 0},
	}
	for _, c := range cases {
		if got := CompareVersions(c.a, c.b); got != c.want {
			t.Errorf("CompareVersions(%q, %q) = %d, want %d", c.a, c.b, got, c.want)
		}
		if got := CompareVersions(c.b, c.a); got != -c.want {
			t.Errorf("CompareVersions(%q, %q) = %d, want %d (antisymmetry)", c.b, c.a, got, -c.want)
		}
	}
}

func TestParseVersionRejects(t *testing.T) {
	for _, s := range []string{"", "1", "1.2", "1.2.3.4", "01.2.3", "1.2.3-", "1.2.3+", "1.2.3-a..b", "1.x.3", "-1.2.3"} {
		if _, err := ParseVersion(s); err == nil {
			t.Errorf("ParseVersion(%q) accepted", s)
		}
	}
}

func TestNewer(t *testing.T) {
	if !Newer("0.1.341", "0.1.340") || Newer("0.1.340", "0.1.340") || Newer("0.1.339", "0.1.340") {
		t.Fatal("Newer is not strict precedence")
	}
}

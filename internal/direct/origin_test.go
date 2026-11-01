package direct

import "testing"

func TestCanonicalOrigin(t *testing.T) {
	good := map[string]string{
		"https://Amux.Home.Example:443": "https://amux.home.example",
		"https://amux.test:8443":        "https://amux.test:8443",
		"https://amux.test/":            "https://amux.test",
		"https://[fd00::1]:8443":        "https://[fd00::1]:8443",
	}
	for in, want := range good {
		got, err := CanonicalOrigin(in, false)
		if err != nil || got != want {
			t.Errorf("%s: %q %v", in, got, err)
		}
	}
	for _, bad := range []string{"http://amux.test", "https://u:p@amux.test", "https://amux.test/api",
		"https://amux.test?x=1", "https://amux.test#f", "https://", " https://amux.test", "amux.test:8443"} {
		if _, err := CanonicalOrigin(bad, false); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	if _, err := CanonicalOrigin("http://localhost:5173", true); err != nil {
		t.Error("dev browser origin refused")
	}
}

func TestCheckBindIsPrivateByDefault(t *testing.T) {
	for _, ok := range []string{"127.0.0.1:8443", "[::1]:8443", "192.168.1.5:8443", "10.0.0.2:1", "172.16.0.1:1",
		"100.64.0.7:8443", "[fd7a:115c:a1e0::1]:8443", "169.254.1.1:1"} {
		if err := CheckBind(ok, false); err != nil {
			t.Errorf("%s refused: %v", ok, err)
		}
	}
	for _, bad := range []string{"0.0.0.0:8443", "[::]:8443", "8.8.8.8:443", ":8443", "amux.local:8443", "127.0.0.1"} {
		if err := CheckBind(bad, false); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
	if err := CheckBind("0.0.0.0:8443", true); err != nil {
		t.Errorf("explicit public bind refused: %v", err)
	}
}

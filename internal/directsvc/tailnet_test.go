package directsvc

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// In tailnet mode the child reloads certificate files itself, so a file
// change must not restart it (that would drop open streams); every setting
// still does. Outside tailnet mode the restart-on-renewal policy is unchanged.
func TestFingerprintTailnetExcludesOnlyReloadedCertFiles(t *testing.T) {
	isolate(t)
	dir := t.TempDir()
	cert, key := filepath.Join(dir, "c.pem"), filepath.Join(dir, "k.pem")
	os.WriteFile(cert, []byte("a"), 0o644)
	os.WriteFile(key, []byte("a"), 0o600)
	tn := Settings{Enabled: true, Listen: "100.64.0.5:8443", TLSCert: cert, TLSKey: key,
		Tailnet: &TailnetSettings{Port: 8443, CertSource: "tailscale"}}
	legacy := tn
	legacy.Tailnet = nil
	fpT, fpL := Fingerprint(tn), Fingerprint(legacy)

	later := time.Now().Add(time.Hour)
	os.WriteFile(cert, []byte("renewed"), 0o644)
	os.Chtimes(cert, later, later)
	if Fingerprint(tn) != fpT {
		t.Fatal("tailnet mode: certificate file change would restart the child")
	}
	if Fingerprint(legacy) == fpL {
		t.Fatal("legacy mode: certificate renewal no longer restarts the child")
	}
	for name, mut := range map[string]func(*Settings){
		"port":        func(s *Settings) { s.Tailnet.Port = 9443 },
		"cert source": func(s *Settings) { s.Tailnet.CertSource = "explicit" },
		"origins":     func(s *Settings) { s.AllowOrigins = []string{"https://h.example"} },
		"listen":      func(s *Settings) { s.Listen = "100.64.0.6:8443" },
	} {
		c := tn
		ts := *tn.Tailnet
		c.Tailnet = &ts
		mut(&c)
		if Fingerprint(c) == fpT {
			t.Fatalf("tailnet mode: %s change does not restart the child", name)
		}
	}
}

func TestTailnetSettingsRoundTrip(t *testing.T) {
	isolate(t)
	s := Settings{Enabled: true, Listen: "100.72.81.114:8443", Endpoints: []string{"https://m.ts.net:8443"},
		Tailnet: &TailnetSettings{Port: 8443, CertSource: "tailscale", DNSName: "m.ts.net", DefaultOrigin: "https://h.example", OriginSource: "x"}}
	if err := Save(s); err != nil {
		t.Fatal(err)
	}
	got, set, err := Load()
	if err != nil || !set || got.Tailnet == nil || *got.Tailnet != *s.Tailnet {
		t.Fatalf("%+v %v %v", got.Tailnet, set, err)
	}
	if a := got.Args(); a[len(a)-3] != "--tailnet" {
		t.Fatalf("args %v", a)
	}
}

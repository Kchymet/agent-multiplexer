//go:build darwin

package panespec

import (
	"amux/internal/launchenv"
	"amux/internal/store"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestSeatbeltRuntimeCertificateBundles(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, "data"))
	t.Setenv("AMUX_JAIL", "on")
	t.Setenv("SSL_CERT_FILE", "")
	t.Setenv("CODEX_CA_CERTIFICATE", "")
	s := store.Session{ID: "certs", Agent: "codex", Dir: filepath.Join(home, "own")}
	spec := testLaunchSpec(t, s)
	run := func(script string) string {
		t.Helper()
		argv, err := scope(s.Dir, TabAgent, s, spec.Access, nil, []string{"/bin/sh", "-c", script})
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(argv[0], argv[1:]...)
		cmd.Dir = s.Dir
		cmd.Env, err = launchenv.Build(os.Environ(), platformLaunchEnv(spec), launchenv.ForRuntime(s.Agent))
		if err != nil {
			t.Fatal(err)
		}
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("certificate probe: %v: %s", err, out)
		}
		return string(out)
	}
	if got := run(`test "$SSL_CERT_FILE" = /etc/ssl/cert.pem && /usr/bin/grep -c 'BEGIN CERTIFICATE' "$SSL_CERT_FILE"`); strings.TrimSpace(got) == "0" {
		t.Fatal("empty default CA bundle")
	}
	custom := filepath.Join(home, "private-certs")
	if err := os.MkdirAll(custom, 0700); err != nil {
		t.Fatal(err)
	}
	cert := filepath.Join(custom, "ca.pem")
	if err := os.WriteFile(cert, []byte("public-test-ca"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(custom, "unrelated"), []byte("private"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"SSL_CERT_FILE", "CODEX_CA_CERTIFICATE"} {
		t.Run(key, func(t *testing.T) {
			t.Setenv(key, cert)
			got := run(`test -z "${SSL_CERT_FILE:-}" || test "$SSL_CERT_FILE" != /etc/ssl/cert.pem; /bin/cat "$` + key + `"; if /bin/cat "` + filepath.Join(custom, "unrelated") + `" 2>/dev/null; then exit 9; fi`)
			if got != "public-test-ca" {
				t.Fatalf("custom CA not propagated: %q", got)
			}
		})
	}
}

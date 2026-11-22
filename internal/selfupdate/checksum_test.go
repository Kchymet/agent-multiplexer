package selfupdate

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func digest(b string) string {
	s := sha256.Sum256([]byte(b))
	return hex.EncodeToString(s[:])
}

func TestParseSums(t *testing.T) {
	a, b := digest("a"), digest("b")
	sums, err := ParseSums([]byte(a + "  amux-linux-amd64\n" + strings.ToUpper(b) + " *amux.sh\n\n"))
	if err != nil {
		t.Fatal(err)
	}
	if sums["amux-linux-amd64"] != a || sums["amux.sh"] != b {
		t.Fatalf("sums = %v", sums)
	}
}

func TestParseSumsRejectsAmbiguousOrMalformed(t *testing.T) {
	a := digest("a")
	for name, in := range map[string]string{
		"duplicate":  a + "  x\n" + a + "  x\n",
		"short":      "abcd  x\n",
		"not hex":    strings.Repeat("z", 64) + "  x\n",
		"no name":    a + "\n",
		"extra cols": a + "  x  y\n",
	} {
		if _, err := ParseSums([]byte(in)); err == nil {
			t.Errorf("%s: accepted %q", name, in)
		}
	}
}

func TestVerifySHA256(t *testing.T) {
	sum := digest("payload")
	if err := VerifySHA256("f", sum, strings.ToUpper(sum)); err != nil {
		t.Fatalf("matching digest rejected: %v", err)
	}
	if err := VerifySHA256("f", sum, digest("other")); err == nil || !strings.Contains(err.Error(), "mismatch") {
		t.Fatalf("mismatch accepted: %v", err)
	}
	if err := VerifySHA256("f", sum, ""); err == nil {
		t.Fatal("an empty expectation must fail, not pass")
	}
}

func TestFileSHA256(t *testing.T) {
	p := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(p, []byte("payload"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := FileSHA256(p)
	if err != nil || got != digest("payload") {
		t.Fatalf("FileSHA256 = %q, %v", got, err)
	}
}

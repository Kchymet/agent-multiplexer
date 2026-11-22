package selfupdate

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"strings"
)

// ParseSums reads a sha256sum-format file ("<hex>  <name>" or "<hex> *<name>")
// into name → lowercase hex digest. A malformed line or a duplicated name is an
// error: a checksum file that cannot be read unambiguously verifies nothing.
func ParseSums(b []byte) (map[string]string, error) {
	sums := map[string]string{}
	sc := bufio.NewScanner(bytes.NewReader(b))
	for line := 1; sc.Scan(); line++ {
		text := strings.TrimSpace(sc.Text())
		if text == "" {
			continue
		}
		fields := strings.Fields(text)
		if len(fields) != 2 {
			return nil, fmt.Errorf("%s line %d: want \"<sha256>  <name>\"", SumsAsset, line)
		}
		digest := strings.ToLower(fields[0])
		name := strings.TrimPrefix(fields[1], "*")
		if len(digest) != sha256.Size*2 {
			return nil, fmt.Errorf("%s line %d: digest is not SHA-256", SumsAsset, line)
		}
		if _, err := hex.DecodeString(digest); err != nil {
			return nil, fmt.Errorf("%s line %d: digest is not hex", SumsAsset, line)
		}
		if _, dup := sums[name]; dup {
			return nil, fmt.Errorf("%s: %s listed twice", SumsAsset, name)
		}
		sums[name] = digest
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return sums, nil
}

// SumReader hashes everything read through it.
func sumOf(r io.Reader) (string, int64, error) {
	h := sha256.New()
	n, err := io.Copy(h, r)
	return hex.EncodeToString(h.Sum(nil)), n, err
}

// FileSHA256 returns the hex SHA-256 of a file.
func FileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	sum, _, err := sumOf(f)
	return sum, err
}

// VerifySHA256 compares a computed digest to the expected one. An empty
// expectation is a failure, never a pass.
func VerifySHA256(name, got, want string) error {
	want = strings.ToLower(strings.TrimSpace(want))
	if want == "" {
		return fmt.Errorf("%s: no expected checksum", name)
	}
	if got != want {
		return fmt.Errorf("%s: checksum mismatch (got %s, want %s)", name, got, want)
	}
	return nil
}

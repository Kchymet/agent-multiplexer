// Command generate creates the deterministic license and notice bundle embedded
// in amux. It inventories the exact Go packages selected for every supported
// release target and copies legal-named files without guessing license IDs.
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

const defaultOutput = "internal/licenses/THIRD_PARTY_NOTICES.txt"

var supportedTargets = []string{
	"darwin/amd64",
	"darwin/arm64",
	"linux/amd64",
	"linux/arm64",
}

type listedPackage struct {
	ImportPath string        `json:"ImportPath"`
	Dir        string        `json:"Dir"`
	Standard   bool          `json:"Standard"`
	Module     *listedModule `json:"Module"`
}

type listedModule struct {
	Path     string        `json:"Path"`
	Version  string        `json:"Version"`
	Sum      string        `json:"Sum"`
	GoModSum string        `json:"GoModSum"`
	Dir      string        `json:"Dir"`
	Main     bool          `json:"Main"`
	Replace  *listedModule `json:"Replace"`
}

type moduleRecord struct {
	Path       string
	Version    string
	Sum        string
	GoModSum   string
	Dir        string
	Packages   map[string]struct{}
	PackageDir map[string]struct{}
	Targets    map[string]struct{}
}

type legalFile struct {
	Path   string
	SHA256 string
	Text   []byte
}

func main() {
	check := flag.Bool("check", false, "fail if the checked-in bundle differs")
	output := flag.String("output", defaultOutput, "output path relative to the module root")
	flag.Parse()

	root, err := moduleRoot()
	if err != nil {
		fatal(err)
	}
	bundle, err := generate(root)
	if err != nil {
		fatal(err)
	}
	path := filepath.Join(root, filepath.FromSlash(*output))
	if *check {
		current, err := os.ReadFile(path)
		if err != nil {
			fatal(err)
		}
		if !bytes.Equal(current, bundle) {
			fatal(errors.New("bundled notices are stale; run `make notices` and commit the result"))
		}
		return
	}
	if err := os.WriteFile(path, bundle, 0o644); err != nil {
		fatal(err)
	}
}

func moduleRoot() (string, error) {
	cmd := exec.Command("go", "env", "GOMOD")
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("locate go.mod: %w", err)
	}
	gomod := strings.TrimSpace(string(out))
	if gomod == "" || gomod == os.DevNull {
		return "", errors.New("generator must run inside the amux module")
	}
	return filepath.Dir(gomod), nil
}

func generate(root string) ([]byte, error) {
	modules := map[string]*moduleRecord{}
	standardVendorRoots := map[string]struct{}{}
	for _, target := range supportedTargets {
		parts := strings.Split(target, "/")
		cmd := exec.Command("go", "list", "-deps", "-json", "./cmd/amux")
		cmd.Dir = root
		cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOFLAGS=", "GOOS="+parts[0], "GOARCH="+parts[1])
		out, stderr, err := commandOutput(cmd)
		if err != nil {
			return nil, fmt.Errorf("list dependencies for %s: %w: %s", target, err, strings.TrimSpace(stderr))
		}
		dec := json.NewDecoder(bytes.NewReader(out))
		for {
			var pkg listedPackage
			if err := dec.Decode(&pkg); errors.Is(err, io.EOF) {
				break
			} else if err != nil {
				return nil, fmt.Errorf("decode go list output for %s: %w", target, err)
			}
			if pkg.Standard && strings.HasPrefix(pkg.ImportPath, "vendor/") {
				parts := strings.Split(pkg.ImportPath, "/")
				if len(parts) >= 4 {
					standardVendorRoots[strings.Join(parts[:4], "/")] = struct{}{}
				}
			}
			if pkg.Standard || pkg.Module == nil || pkg.Module.Main {
				continue
			}
			mod := pkg.Module
			dir := mod.Dir
			if mod.Replace != nil && mod.Replace.Dir != "" {
				dir = mod.Replace.Dir
			}
			rec := modules[mod.Path]
			if rec == nil {
				rec = &moduleRecord{
					Path: mod.Path, Version: mod.Version, Sum: mod.Sum, GoModSum: mod.GoModSum, Dir: dir,
					Packages: map[string]struct{}{}, PackageDir: map[string]struct{}{}, Targets: map[string]struct{}{},
				}
				modules[mod.Path] = rec
			}
			rec.Packages[pkg.ImportPath] = struct{}{}
			rec.PackageDir[pkg.Dir] = struct{}{}
			rec.Targets[target] = struct{}{}
		}
	}

	var buf bytes.Buffer
	fmt.Fprintln(&buf, "AMUX DISTRIBUTED LICENSE AND NOTICE MATERIAL")
	fmt.Fprintln(&buf)
	fmt.Fprintln(&buf, "This file is generated from the exact packages selected by `go list -deps`")
	fmt.Fprintln(&buf, "for the supported release targets listed below. It records build provenance and")
	fmt.Fprintln(&buf, "copies upstream legal-named files verbatim; it does not infer SPDX identifiers,")
	fmt.Fprintln(&buf, "ownership, compatibility, trademark rights, or legal/commercial clearance.")
	fmt.Fprintln(&buf)
	fmt.Fprintf(&buf, "Supported targets: %s\n", strings.Join(supportedTargets, ", "))
	fmt.Fprintf(&buf, "Go toolchain: %s\n", goEnv("GOVERSION"))
	fmt.Fprintln(&buf, "First-party module paths excluded from the third-party inventory: amux and")
	fmt.Fprintln(&buf, "github.com/kchymet/agent-multiplexer/harnessproto. The repository root license")
	fmt.Fprintln(&buf, "is reproduced separately below; source alone does not verify ownership facts.")

	paths := make([]string, 0, len(modules))
	for path := range modules {
		if !isFirstParty(path) {
			paths = append(paths, path)
		}
	}
	sort.Strings(paths)
	fmt.Fprintf(&buf, "\n=== RESOLVED THIRD-PARTY GO MODULES (%d) ===\n", len(paths))
	for _, path := range paths {
		rec := modules[path]
		if rec.Version == "" || rec.Sum == "" {
			return nil, fmt.Errorf("third-party module %s lacks version/checksum provenance", rec.Path)
		}
		files, err := collectLegalFiles(rec)
		if err != nil {
			return nil, err
		}
		if len(files) == 0 {
			return nil, fmt.Errorf("third-party module %s@%s has no legal-named file; review required", rec.Path, rec.Version)
		}
		fmt.Fprintf(&buf, "\n%s@%s\n", rec.Path, rec.Version)
		fmt.Fprintf(&buf, "  module checksum: %s\n", rec.Sum)
		fmt.Fprintf(&buf, "  go.mod checksum: %s\n", rec.GoModSum)
		fmt.Fprintf(&buf, "  targets: %s\n", joinSet(rec.Targets))
		fmt.Fprintf(&buf, "  linked packages: %s\n", strings.Join(sortedKeys(rec.Packages), ", "))
		fmt.Fprintf(&buf, "  legal files: %s\n", legalFileSummary(files))
	}

	fmt.Fprintln(&buf, "\n=== EXACT UPSTREAM LEGAL FILES ===")
	for _, path := range paths {
		rec := modules[path]
		files, err := collectLegalFiles(rec)
		if err != nil {
			return nil, err
		}
		for _, file := range files {
			fmt.Fprintf(&buf, "\n--- %s@%s :: %s (SHA-256 %s) ---\n", rec.Path, rec.Version, file.Path, file.SHA256)
			buf.Write(file.Text)
			if len(file.Text) == 0 || file.Text[len(file.Text)-1] != '\n' {
				buf.WriteByte('\n')
			}
		}
	}

	for _, vendorRoot := range sortedKeys(standardVendorRoots) {
		dir := filepath.Join(runtimeRoot(), "src", filepath.FromSlash(vendorRoot))
		entries, err := os.ReadDir(dir)
		if err != nil {
			return nil, fmt.Errorf("read toolchain vendor root %s: %w", vendorRoot, err)
		}
		found := 0
		for _, entry := range entries {
			if entry.IsDir() || !isLegalName(entry.Name()) {
				continue
			}
			found++
			display := filepath.ToSlash(filepath.Join("GOROOT/src", vendorRoot, entry.Name()))
			heading := "GO TOOLCHAIN VENDORED PACKAGE MATERIAL: " + vendorRoot + "/" + entry.Name()
			if err := appendLocalFile(&buf, heading, filepath.Join(dir, entry.Name()), display); err != nil {
				return nil, err
			}
		}
		if found == 0 {
			return nil, fmt.Errorf("toolchain vendor root %s has no legal-named file; review required", vendorRoot)
		}
	}

	goLicense, err := runtimeLegalPath("LICENSE")
	if err != nil {
		return nil, err
	}
	if err := appendLocalFile(&buf, "GO TOOLCHAIN LICENSE MATERIAL", goLicense, "GOROOT/LICENSE"); err != nil {
		return nil, err
	}
	patents, patentsErr := runtimeLegalPath("PATENTS")
	if patentsErr == nil {
		if err := appendLocalFile(&buf, "GO TOOLCHAIN PATENT NOTICE", patents, "GOROOT/PATENTS"); err != nil {
			return nil, err
		}
	} else if !errors.Is(patentsErr, fs.ErrNotExist) {
		return nil, patentsErr
	}
	if err := appendLocalFile(&buf, "REPOSITORY ROOT LICENSE", filepath.Join(root, "LICENSE"), "LICENSE"); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// commandOutput keeps diagnostic/status stderr separate from machine-readable
// stdout. In particular, a cold Go module cache prints successful
// "go: downloading ..." lines to stderr while go list emits JSON to stdout.
func commandOutput(cmd *exec.Cmd) ([]byte, string, error) {
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	return out, stderr.String(), err
}

func collectLegalFiles(rec *moduleRecord) ([]legalFile, error) {
	dirs := map[string]struct{}{rec.Dir: {}}
	for dir := range rec.PackageDir {
		dirs[dir] = struct{}{}
	}
	byPath := map[string]legalFile{}
	for dir := range dirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", dir, err)
		}
		for _, entry := range entries {
			if entry.IsDir() || !isLegalName(entry.Name()) {
				continue
			}
			full := filepath.Join(dir, entry.Name())
			text, err := os.ReadFile(full)
			if err != nil {
				return nil, fmt.Errorf("read %s: %w", full, err)
			}
			rel, err := filepath.Rel(rec.Dir, full)
			if err != nil || strings.HasPrefix(rel, "..") {
				return nil, fmt.Errorf("legal file %s is outside module %s", full, rec.Path)
			}
			sum := sha256.Sum256(text)
			byPath[filepath.ToSlash(rel)] = legalFile{Path: filepath.ToSlash(rel), SHA256: hex.EncodeToString(sum[:]), Text: text}
		}
	}
	paths := make([]string, 0, len(byPath))
	for path := range byPath {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	files := make([]legalFile, 0, len(paths))
	for _, path := range paths {
		files = append(files, byPath[path])
	}
	return files, nil
}

func isFirstParty(path string) bool {
	return path == "amux" || path == "github.com/kchymet/agent-multiplexer/harnessproto"
}

func isLegalName(name string) bool {
	upper := strings.ToUpper(name)
	for _, prefix := range []string{"LICENSE", "LICENCE", "NOTICE", "COPYING", "COPYRIGHT", "PATENTS"} {
		if upper == prefix || strings.HasPrefix(upper, prefix+".") {
			return true
		}
	}
	return false
}

func legalFileSummary(files []legalFile) string {
	parts := make([]string, 0, len(files))
	for _, file := range files {
		parts = append(parts, file.Path+" (SHA-256 "+file.SHA256+")")
	}
	return strings.Join(parts, ", ")
}

func appendLocalFile(buf *bytes.Buffer, heading, path, display string) error {
	text, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	sum := sha256.Sum256(text)
	fmt.Fprintf(buf, "\n=== %s ===\n%s (SHA-256 %x)\n\n", heading, display, sum)
	buf.Write(text)
	if len(text) == 0 || text[len(text)-1] != '\n' {
		buf.WriteByte('\n')
	}
	return nil
}

func runtimeRoot() string {
	return goEnv("GOROOT")
}

func goEnv(name string) string {
	cmd := exec.Command("go", "env", name)
	out, err := cmd.Output()
	if err != nil {
		fatal(fmt.Errorf("read go env %s: %w", name, err))
	}
	return strings.TrimSpace(string(out))
}

func runtimeLegalPath(name string) (string, error) {
	root := runtimeRoot()
	for _, path := range []string{filepath.Join(root, name), filepath.Join(filepath.Dir(root), name)} {
		if _, err := os.Stat(path); err == nil {
			return path, nil
		} else if !errors.Is(err, fs.ErrNotExist) {
			return "", err
		}
	}
	return "", fmt.Errorf("%s: %w", name, fs.ErrNotExist)
}

func sortedKeys(set map[string]struct{}) []string {
	keys := make([]string, 0, len(set))
	for key := range set {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func joinSet(set map[string]struct{}) string { return strings.Join(sortedKeys(set), ", ") }

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "generate notices:", err)
	os.Exit(1)
}

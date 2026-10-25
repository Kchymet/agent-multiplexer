package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestCommandOutputKeepsSuccessfulStderrOutOfJSON(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=TestGeneratorHelperProcess")
	cmd.Env = append(os.Environ(), "AMUX_GENERATOR_HELPER=1")
	out, stderr, err := commandOutput(cmd)
	if err != nil {
		t.Fatalf("commandOutput: %v", err)
	}
	var pkg listedPackage
	if err := json.Unmarshal(out, &pkg); err != nil {
		t.Fatalf("decode stdout JSON: %v\n%s", err, out)
	}
	if pkg.ImportPath != "example.test/dependency" {
		t.Fatalf("import path = %q", pkg.ImportPath)
	}
	if !strings.Contains(stderr, "go: downloading example.test/dependency") {
		t.Fatalf("stderr = %q", stderr)
	}
}

func TestGeneratorHelperProcess(t *testing.T) {
	if os.Getenv("AMUX_GENERATOR_HELPER") != "1" {
		return
	}
	fmt.Fprintln(os.Stdout, `{"ImportPath":"example.test/dependency"}`)
	fmt.Fprintln(os.Stderr, "go: downloading example.test/dependency v1.0.0")
	os.Exit(0)
}

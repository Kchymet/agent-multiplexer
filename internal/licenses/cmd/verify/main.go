package main

import (
	"debug/buildinfo"
	"flag"
	"fmt"
	"os"

	"amux/internal/licenses"
)

func main() {
	binary := flag.String("binary", "bin/amux", "path to the built amux binary")
	flag.Parse()
	info, err := buildinfo.ReadFile(*binary)
	if err != nil {
		fatal(err)
	}
	if err := licenses.VerifyBuildInfo(info, licenses.Text()); err != nil {
		fatal(err)
	}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "verify notice provenance:", err)
	os.Exit(1)
}

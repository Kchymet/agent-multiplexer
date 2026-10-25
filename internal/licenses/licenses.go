// Package licenses exposes the exact license and notice bundle generated from
// the modules linked into the supported amux binaries.
package licenses

import _ "embed"

//go:generate go run ./cmd/generate

//go:embed THIRD_PARTY_NOTICES.txt
var text string

// Text returns the bundled license and notice material.
func Text() string { return text }

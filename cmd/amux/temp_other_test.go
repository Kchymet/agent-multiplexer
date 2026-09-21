//go:build !darwin

package main

// useCanonicalTestTempDir is a no-op outside Darwin: the default temp
// directory is already a short, canonical path.
func useCanonicalTestTempDir() error { return nil }

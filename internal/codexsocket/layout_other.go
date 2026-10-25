//go:build !linux

package codexsocket

// PrepareNamespaceMount is Linux-only. Other platforms keep their existing
// transport policy (including macOS's exact Seatbelt grants).
func PrepareNamespaceMount(string) (source, target string, err error) { return "", "", nil }

// ResolveDialPath leaves non-Linux endpoint handling unchanged.
func ResolveDialPath(rendezvous string) (string, error) { return rendezvous, nil }

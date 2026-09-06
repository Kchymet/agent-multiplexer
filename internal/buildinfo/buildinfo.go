// Package buildinfo holds the version contracts shared by the amux CLI and
// daemon. Version may be replaced at link time; the protocol number changes
// only when a CLI and daemon can no longer safely speak the same wire format.
package buildinfo

// Version is the product version of this binary. Release builds replace it with
// -X amux/internal/buildinfo.Version=<version>.
var Version = "0.1.0"

// DaemonProtocol is the local CLI-to-daemon API contract. Additive changes may
// keep the same number; bump it when either side must reject the other.
const DaemonProtocol = 1

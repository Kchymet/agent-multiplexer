package main

import (
	"fmt"

	"amux/internal/mux"
)

// cmdServe runs the authenticated legacy multiplexer relay. Extra args may add
// TLS listeners, e.g. `amux serve tls:0.0.0.0:7443`; the default Unix listener
// is TLS-wrapped too. Certificate/key, client trust, and the mandatory bearer
// come from AMUX_TLS_* / AMUX_MUX_TOKEN. See docs/client-server.md.
func cmdServe(args []string) error { return mux.Run(args...) }

// cmdHarness used to expose arbitrary process spawning over unauthenticated
// stdio. Legacy mux now bridges primary-daemon-owned panes and has no embedded
// harness; no standalone CLI trust assertion can substitute for authentication.
func cmdHarness() error {
	return fmt.Errorf("standalone harness requires an authenticated inherited host channel")
}

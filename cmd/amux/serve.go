package main

import "amux/internal/mux"

// cmdServe runs the authenticated legacy multiplexer relay. Extra args may add
// TLS listeners, e.g. `amux serve tls:0.0.0.0:7443`; the default Unix listener
// is TLS-wrapped too. Certificate/key, client trust, and the mandatory bearer
// come from AMUX_TLS_* / AMUX_MUX_TOKEN. See docs/client-server.md.
func cmdServe(args []string) error { return mux.Run(args...) }

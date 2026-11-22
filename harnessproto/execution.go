package harnessproto

// ExecutionCapabilities describes the workloads and credential sources a pool
// supports. An absent block is a legacy provider; an empty Harnesses list in a
// present block means no harness was verified. Never infer Claude in that case.
// No credentials or account identifiers belong in this advertisement.
type ExecutionCapabilities struct {
	Harnesses     []HarnessCapability `json:"harnesses"`
	IdentityModes []string            `json:"identityModes"`
}

type HarnessCapability struct {
	Name    string `json:"name"`
	Version string `json:"version,omitempty"`
	// Models is the harness's selectable model list on this provider, the
	// first entry its default. It carries only plain public model names
	// (provider paths that may embed account or project identifiers are
	// filtered out). Absent means the provider did not advertise one, not that
	// the harness has no models.
	Models []string `json:"models,omitempty"`
}

const (
	IdentityMachine = "machine" // inherit the execution machine's global configuration
	IdentityAPIKey  = "api-key" // credentials supplied by the pool
)

func (c *ExecutionCapabilities) Supports(harness, identity string) bool {
	if c == nil {
		return false
	}
	found := false
	for _, h := range c.Harnesses {
		if h.Name == harness {
			found = true
			break
		}
	}
	if !found {
		return false
	}
	for _, mode := range c.IdentityModes {
		if mode == identity {
			return true
		}
	}
	return false
}

package direct

// ResourceScope is verbatim from 15fcf0e internal/direct/wire.go.
type ResourceScope struct {
	Kind       string   `json:"kind"`                 // all_sessions | workgroups
	Workgroups []string `json:"workgroups,omitempty"` // root ids, for kind workgroups
}

// Identity is opaque here: the frozen grant reader never inspects it.
type Identity struct{ raw []byte }

func unmarshalIdentity(data []byte) (*Identity, error) { return &Identity{raw: data}, nil }

func (id *Identity) marshal() ([]byte, error) { return id.raw, nil }

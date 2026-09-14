package hostprep

import "path/filepath"

// OpenAbsolute pins an existing directory without following aliases in any
// component. Discovery uses it because even a private home's parent components
// may be replaced by the session before a host-side scan.
func OpenAbsolute(name string) (*Root, error) {
	abs, err := filepath.Abs(name)
	if err != nil {
		return nil, err
	}
	root, err := OpenSession(string(filepath.Separator))
	if err != nil {
		return nil, err
	}
	rel, err := filepath.Rel(string(filepath.Separator), abs)
	if err != nil {
		root.Close()
		return nil, err
	}
	if rel == "." {
		return root, nil
	}
	defer root.Close()
	return root.Sub(rel, false, 0)
}

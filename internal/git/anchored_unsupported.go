//go:build !linux && !darwin

package git

import "fmt"

func publishCheckout(_, _, _ string) error {
	return fmt.Errorf("independent checkout publication is unsupported on this operating system")
}

func removeTreeAnchored(_, _, _ string) error {
	return fmt.Errorf("anchored checkout removal is unsupported on this operating system")
}

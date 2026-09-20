//go:build !linux && !darwin

package hostprep

import (
	"errors"
	"os"
)

func linkExternal(string, *os.File, string) error {
	return errors.New("hostprep: descriptor-anchored external hard links are unsupported on this platform")
}

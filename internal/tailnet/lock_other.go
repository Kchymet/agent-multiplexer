//go:build !unix

package tailnet

import "errors"

func lockFile(string) (func(), error) {
	return nil, errors.New("the tailnet certificate store is supported only on Unix")
}

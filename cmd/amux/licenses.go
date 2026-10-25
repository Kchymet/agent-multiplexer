package main

import (
	"fmt"

	"amux/internal/licenses"
)

func cmdLicenses(args []string) error {
	if len(args) != 0 {
		return fmt.Errorf("usage: amux licenses")
	}
	fmt.Print(licenses.Text())
	return nil
}

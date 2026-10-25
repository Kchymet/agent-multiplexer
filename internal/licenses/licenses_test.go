package licenses

import (
	"strings"
	"testing"
)

func TestBundleContainsInventoryAndExactTexts(t *testing.T) {
	for _, want := range []string{
		"AMUX DISTRIBUTED LICENSE AND NOTICE MATERIAL",
		"RESOLVED THIRD-PARTY GO MODULES (38)",
		"github.com/charmbracelet/bubbletea@v1.3.10",
		"modernc.org/sqlite@v1.53.0",
		"GO TOOLCHAIN VENDORED PACKAGE MATERIAL: vendor/golang.org/x/crypto/LICENSE",
		"GO TOOLCHAIN LICENSE MATERIAL",
		"REPOSITORY ROOT LICENSE",
	} {
		if !strings.Contains(Text(), want) {
			t.Errorf("bundled notices missing %q", want)
		}
	}
}

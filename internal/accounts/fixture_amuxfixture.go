//go:build amuxfixture

package accounts

import "amux/internal/amuxcfg"

// Native runtime tests built with -tags amuxfixture drive real harnesses at a
// local deterministic provider, which central auth (pinned official endpoints)
// cannot serve. Only a binding the fixture itself marks routes to the
// inherited environment; release builds never compile this file.
func init() {
	fixtureSource = func(b Binding) string {
		if b.Origin == FixtureOrigin && b.Source == amuxcfg.AuthEnvironment {
			return amuxcfg.AuthEnvironment
		}
		return ""
	}
}

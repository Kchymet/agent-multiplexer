package agent

// hermesHarness implements Harness for the Hermes CLI. amux can launch it, but it
// exposes no hook stream, no resume protocol amux drives, and no config amux
// manages — so beyond Argv it takes the vendor-neutral no-op defaults (no
// activity signal, fresh launch, .agents/skills + AGENTS.md). Registering it here
// makes it a first-class (if minimal) harness instead of a half-supported kind
// that Argv could launch but nothing else understood.
type hermesHarness struct{ noopHarness }

func (hermesHarness) Kind() string { return "hermes" }

// Argv launches `hermes chat`, passing the model via -m when set.
func (hermesHarness) Argv(model string, extra ...string) ([]string, error) {
	bin := envOr("AMUX_HERMES_BIN", "hermes")
	args := []string{"chat"}
	if model != "" {
		args = append(args, "-m", model)
	}
	return finishArgv(bin, args, extra), nil
}

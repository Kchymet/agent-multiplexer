# Contributing

amux is experimental. Open an issue for a bug or proposed change, or submit a
focused pull request against `master`. For sensitive findings, follow
[Security](SECURITY.md#reporting-a-suspected-vulnerability).

## Development setup

Use Go 1.26.8 or a newer patched release and Git. Runtime tests need native
macOS Seatbelt or Linux/WSL2 with bubblewrap 0.12.0+ and usable user/PID
namespaces. Native Windows is unsupported. See the
[installation guide](README.md#install) and [runtime validation](docs/agent-cli-validation.md).

```sh
make build
make vet
make test
make vuln
make cross
```

`make test`, `make vet` and `make vuln` cover both the root module and the
separately published `harnessproto` module. Running only `go test ./...` at the
root misses that nested module. `make vuln` downloads a pinned Go vulnerability
checker and queries the Go vulnerability database; it uses the root module's
selected toolchain for both modules. `make cross` checks compilation for macOS
and Linux on arm64/amd64; it does not execute those binaries.

`make fmt` applies gofmt. CI checks formatting, vet, tests, vulnerabilities and
real OS/harness boundaries. A local test can skip when a required runtime is
missing: a skip does not establish that the boundary passed. See the runtime
validation guide for the required fixtures and environment variables.

## Live harness checks

Normal tests use temporary state and synthetic inputs. The pinned harness tests
in CI use local deterministic model endpoints; they do not need your model
account. Some additional checks are opt-in:

- `make test-live` uses authenticated installed harnesses and costs model turns.
- Browser/network checks in the sandbox guide open a browser or contact public
  endpoints. Run them from a host terminal that can launch the OS sandbox.
- Restart smoke tests are described in
  [Codex supervision](docs/codex-app-server-supervision.md#restart-and-goal-continuation).

Do not use production credentials or real private transcripts as test fixtures.
Generate temporary keys/certificates where needed and use explicit fake tokens.
Review `git diff --cached` before committing. Keep local scanner reports outside
the checkout, and redact them before sharing.

## Pull requests

Explain the observed problem, the change, and the checks actually run. For
changes to permissions, credentials, runtime ownership or transport, describe
the affected boundary and add a focused regression test. For CLI or configuration
changes, update both command help and the relevant user guide. Mention upgrade
and restart requirements. Keep historical validation records dated rather than
turning an old result into a claim about a new version.

The repository uses the [MIT license](LICENSE). Contributions are provided
under that license.

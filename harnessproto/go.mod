// Module github.com/kchymet/agent-multiplexer/harnessproto is the published,
// importable wire protocol for the Multiplexer Server <-> Agent Harness link
// (v1) and its remote-provider extension (v2). It is deliberately a nested
// module with a stdlib-only dependency surface so a separate repo (harness) can
// import the canonical types instead of hand-mirroring them. See ../docs/
// remote-provider.md and ../docs/remote-provider-sessions.md.
module github.com/kchymet/agent-multiplexer/harnessproto

go 1.25.0

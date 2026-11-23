# AGENTS.md: instructions for coding agents working on amux

These instructions are tool-agnostic: Claude Code (via CLAUDE.md), Codex,
Gemini and Antigravity, Cursor, and amux's own agents all read this file. Keep
it short. Put details in `docs/` and link them from here.

## Before you change these areas, read the linked doc

- **Steering / typing into agent panes** (`internal/daemon/typed_delivery.go`,
  `internal/agent/keys.go`, anything that sends text to an agent):
  [`docs/steering.md`](docs/steering.md). In particular, Claude Code steering
  text is TYPED, never pasted: a paste becomes untrusted `<pasted_content>`.
  amux never leaves its own text behind in a composer. Composer checks compare
  against what typing renders, in the input region only.
- **The daemon is a single point of failure on every critical path**
  (`internal/daemon`, `internal/sessionrpc`): keep hot paths lightweight and
  lock-light, never hold a global lock across I/O, git or typing, keep telemetry
  off the critical path, and give every durable per-request artifact a bounded
  lifetime and a reclaimer. Background: the 2026-11-22 response-budget livelock
  (#336). A detailed incident doc and the full hot-path rules are being added
  by the daemon contention work. Extend this section rather than adding a second
  one.

## Working conventions

- One PR per change; the maintainer merges (no auto-merge). CI minutes are
  limited: push as few times as possible.
- CLI help: `amux help --all` (or `amux --help-all`) prints the entire command
  tree in one call.

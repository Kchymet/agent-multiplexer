# Architecture overview

A one-screen map of where amux sits next to [Harness](https://github.com/Kchymet/harness),
the cloud orchestrator it can attach to. The boxes are logical roles, not
services: one amux daemon fills the amux control API, the local session
manager, and the host backend at once. The same diagram lives in the Harness
repo as
[docs/architecture-overview.md](https://github.com/Kchymet/harness/blob/master/docs/architecture-overview.md),
which also explains the Harness-side roles, registration and pooling.

## Diagram

```
┌────────────────────────────┐    ┌────────────────────────────┐
│ Cloud portal clients       │    │ Local clients              │
│ Harness web SPA · Android  │    │ amux TUI · amux CLI        │
└──────────────┬─────────────┘    └──────────────┬─────────────┘
               ↕ /api/ws snapshot+actions        ↕ authenticated local RPC
┌──────────────┴─────────────┐    ┌──────────────┴─────────────┐
│ Harness control API        │    │ amux control API           │
│ auth, fleet, pools, tokens │    │ workgroups, agents, provide│
└───┬──────────────────┬─────┘    └─────┬──────────────────┬───┘
    ↕ control          │                │                  ↕ localhost
    │                  └───────┬────────┘                  │ request /
    │                          ↕ request / schedule agent  │ control
    │           ┌──────────────┴───────────────┐           │
    │           │ Compute-pool registry        │           │
    │           │ API: catalog, tokens, policy │           │
    │           │ wire: harnessproto v2 / TLS  │           │
    │           └──────────────┬───────────────┘           │
    │                          ↕ compute registration      │
┌───┴──────────────────────────┴───────────────────────────┴───┐
│ Compute broker: sessions, event log, placement onto backend  │
│ cloud: Harness broker role on k8s · local: the amux daemon   │
└──────┬─────────────────────────┬─────────────────────────┬───┘
       ↕ commands/status+events  ↕                         ↕
┌──────┴───────────┐  ┌──────────┴─────────────┐  ┌────────┴──────────┐
│ Cloud execution  │  │ amux host backend      │  │ Other provider    │
│ k8s agent pods   │  │ amux daemon panes in   │  │ any harnessproto  │
│ e.g. Claude Code,│  │ bwrap / macOS sandbox  │  │ v2 speaker over   │
│ Codex App Server │  │ e.g. Claude Code, Codex│  │ outbound TLS      │
└──────────────────┘  └────────────────────────┘  └───────────────────┘
```

Every arrow is two-way: commands flow toward the backends, status and events
flow back toward the clients.

- **Local amux works without the cloud.** The right-hand column is the
  standalone path: the TUI and CLI control their own daemon, which manages its
  own sessions, worktrees and sandboxed panes with no Harness involved. The
  daemon's local socket and the authenticated mailbox RPC used by agent-scoped
  `amux agent …` commands are described in
  [Local daemon and optional legacy relay](client-server.md) and
  [Agent CLI access and authorization](agent-cli-sandbox.md). What a
  workgroup's coordinator and members may do over that RPC is mapped in
  [Workgroup permissions](workgroup-permissions.md).
- **The provider link is optional and outbound.** `amux provide` dials the
  Harness broker over one long-lived TLS connection and serves the published
  `harnessproto` v2 protocol on it: spawn, input, resize and kill commands come
  down; pane output, exit, session inventory and runtime events go back up. The
  `compute` grant for running work is separate from publishing sessions and
  accepting session controls. See
  [amux as a remote compute provider](remote-provider.md) and
  [Provider session, steering and runtime-event protocol](remote-provider-sessions.md).
- **Local requests never touch the registry.** The amux API's outer path is
  localhost request / control: the TUI and CLI create and control agents on
  their own daemon directly, with no enrollment and no pool involved. That
  holds whether or not the machine is also enrolled with Harness.
- **The compute-pool registry is two surfaces, not one service.** On the
  Harness control API it is the pool catalog, enrollment tokens and placement
  requests (`pool.*` and `provider.*` actions; the catalog lives in Harness's
  Postgres). On the amux side it is `amux provide`: the daemon presents the
  enrollment token to the broker over TLS, advertises labels, features and the
  harnesses it can run, and receives agents scheduled onto it over the joined
  request / schedule agent path. The `compute` flag in that registration is
  what makes the broker treat the machine as a placement engine, which is the
  compute registration edge in the diagram. An **owned**, non-revoked
  registration is itself a pool whose id is the `providerId` that
  `amux doctor` prints; a legacy unowned daemon has no machine pool. Grouped
  and organization pools are made on Harness, not in amux. See
  [Compute pools and machines](remote-provider.md#compute-pools-and-machines).
- **Session runtimes** such as Claude Code and Codex App Server run as
  processes on a backend. amux resolves them per agent kind and scopes each
  pane to its worktree with bubblewrap on Linux/WSL2 or Seatbelt on macOS; see
  the component table in the [README](../README.md#components).

# Connection access and the host backend

How amux fits the cross-repo connection vocabulary shared with Harness and the
Android client, and which amux code owns each part.

## Shared vocabulary (defined by clients, not by amux)

Harness and Android describe where a client can reach a backend with a
versioned descriptor (`schema_version: 1`):

- `backend_id` is an **opaque, stable, local catalog identity** owned by the
  client that stores it. It is not a URL, it does not prove the destination's
  identity, and it is not a global machine identifier. A client may generate
  one for its migrated Harness profile.
- `kind` is `harness` or `amux`.
- `routes[]` lists the ways to reach that backend. A route is identified by the
  pair (`backend_id`, `route_id`). Routes never define resource identity.
  `access` is `harness-relay` or `direct`. A direct route may carry
  Headscale network metadata (`kind: "headscale"`, `control_url`, `hostname`,
  `tailnet`), but that is metadata only: it claims no enrollment and no login.
- A route's `credential.scope` is `harness-session` or `backend`. A descriptor
  never carries secrets.
- A route's `availability` is `available` or `unavailable` with a reason
  (`not_implemented`, `not_configured`, `network_enrollment_required`).

Clients qualify resource and cache keys (scope, owner, session ids) with
`backend_id`. A Harness profile's provider ids are aliases local to that
profile, not stable machine ids.

## What amux serves today

| Path | Access | Owner in amux |
| --- | --- | --- |
| Native TUI and CLI → local daemon | internal native path (not a descriptor route) | `internal/hostbackend` → `daemon.DialContext` |
| Harness web and Android → this host's sessions | `harness-relay`, through Harness | `amux provide`: `provider.HarnessRelay` transport + `internal/hostbackend` operations |
| Legacy compatibility relay (`amux serve`) | legacy shared-token mux | `internal/mux` → `internal/hostbackend` |
| `direct` to amux | **not implemented**: no listener, endpoint, enrollment or credential exists | none |

The provider's outbound link is a **provider transport**, not a client direct
route. `amux provide` dials the broker over TLS, registers with its bearer
token, and serves harnessproto v2 on that link (see
[remote-provider.md](remote-provider.md)). It advertises no endpoint, listener
or machine identity. The `providerId` the broker assigns is the broker's
alias for this registration, not a global machine id.

## The host backend seam

`internal/hostbackend.HostDaemon` holds the session operations every transport
needs: inventory snapshot, lifecycle/control apply, runtime-record
resolution, account connections and pane streams. Transports reach the local
daemon only through it:

- **Provider (production Harness relay):** `cmd/amux/provide.go`
  `wireHostBackend` routes the provider's session, verb, runtime-event and
  account hooks here. The provider's own grants (`--publish-sessions`,
  `--read-only-sessions`, `--runtime-events`, account status/use,
  `--allow-compute`) still decide which hooks exist and which verbs are
  admitted.
- **Legacy mux:** `mux.daemonPrimary` delegates snapshot, dispatch and panes.
- **Native TUI:** the dashboard stream and its account connection open through
  `Connect`/`ConnectContext`.

Authority is unchanged. Each one-shot operation opens a fresh connection that
verifies the daemon's pinned TLS identity and proves the current host
credential, so rotation or revocation takes effect on the next call.
Cancellation closes the authenticated transport and waits for the in-flight
callback before returning. A late result is discarded, but an effect the daemon
already committed is not rolled back.

## Not yet built

Direct access to amux needs, at minimum: a listener, destination-issued
(`backend`-scope) credentials and enrollment, server-issued backend identity
with authenticated alias binding, and a way to publish it as a route. Harness
Headscale OIDC login is also future work. None of it is implied by the code
above.

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
| Native TUI → local daemon | internal native path (not a descriptor route) | `internal/hostbackend` → `daemon.DialContext` |
| Harness web and Android → this host's sessions | `harness-relay`, through Harness | `amux provide`: `provider.HarnessRelay` transport + `internal/hostbackend` operations |
| Legacy compatibility relay (`amux serve`) | legacy shared-token mux | `internal/mux` → `internal/hostbackend` |
| `direct` to amux (S1) | opt-in `amux direct serve`: destination-issued grants, paste enrollment, read + prompt/stop | `internal/direct` → `internal/hostbackend` (see [direct-access.md](direct-access.md)) |

The provider's outbound link is a **provider transport**, not a client direct
route. `amux provide` dials the broker over TLS, registers with its bearer
token, and serves harnessproto v2 on that link (see
[remote-provider.md](remote-provider.md)). Optional
[cloud-assisted discovery/enrollment](direct-cloud-enrollment.md) advertises
the direct server's `server_id` and HTTPS endpoints over that provider link;
direct session traffic goes from the client to `amux direct serve`.
The `providerId` the broker assigns is the broker's
alias for this registration, not a global machine id.

`amux provide revoke --yes` revokes that token's broker registration and relay
access through a standalone authenticated TLS exchange. **Existing direct-client
grants remain valid**, and agent-session credentials are separate. See
[provider revocation](remote-provider.md#revoke-this-machines-registration-and-relay-access)
for acknowledgment guarantees, retries, and local service cleanup.

## The host backend seam

`internal/hostbackend.HostDaemon` holds the session operations every transport
needs: inventory snapshot, lifecycle/control apply, runtime-record
resolution, account connections and pane streams. The provider, the legacy
mux and the native TUI reach the local daemon only through it. Other CLI
commands still dial the daemon (or session RPC) directly; this seam does not
cover them yet.

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

## Direct access

[direct-access.md](direct-access.md) describes S1: the listener, the server
identity (`server_id`, separate from any client's local `backend_id`), the
destination-issued credentials, manual paste enrollment, and the wire fixtures
under `docs/direct/fixtures/v1`.

## Network routing

Direct access uses a LAN or VPN/tailnet route the operator already has.
amux does not enroll the machine in that network.

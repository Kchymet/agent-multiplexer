# Direct revoke-barrier test publication fix

## Scope and finding

This test-only follow-up is based on exact host master
`c0e06c5fcee261ef795ad37f4095a323a53b45fe`. Host run `41359837670`
failed `internal/direct.TestRevokeBarrierAdmitsNoNewPrivilegedCall` because the
test used a non-blocking receive immediately after `Authority.Revoke` returned.

Source inspection found no demonstrated revocation-barrier bypass. `Revoke`
cancels admitted leases and waits for each `Lease.Done`. The blocking fake
backend returns when that cancellation reaches its execution context, and the
HTTP handler releases the lease before returning. Only then does the outer test
goroutine publish its recorder to the `inflight` channel; that last send is not
part of the authority barrier and can legitimately occur after `Revoke`
returns.

## Change

- Instrument the fake backend with an atomic count of currently executing
  `Apply` calls.
- After successful revocation, require both zero active fake `Apply` calls and
  zero active leases on the revoked grant.
- Await the handler recorder publication with a five-second bound instead of
  racing it with a `default` select.
- Retain the assertions that later victim requests cannot reach either snapshot
  or apply backends, and require that an independent eligible grant reaches
  `Apply` exactly once.

No production authority, admission, backend, wire, deadline, or permission code
changed. Open PR #278 was inspected for overlap; it owns cloud-assisted
enrollment and does not contain this test-fixture correction.

## Verification and limits

- `go test ./internal/direct -run '^TestRevokeBarrierAdmitsNoNewPrivilegedCall$' -count=100`
  passed.
- `go test -race ./internal/direct -run '^TestRevokeBarrierAdmitsNoNewPrivilegedCall$' -count=20`
  passed.

No daemon, provider, browser, account, or model process was run. Authenticated
amux status selected/stored `gpt-5.6-sol`; high reasoning is the requested
setting. No serving-side runtime identity is asserted.

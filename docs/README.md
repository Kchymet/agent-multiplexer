# Documentation

## Using amux

- [Install and first session](../README.md#install)
- [Security and trust boundaries](../SECURITY.md)
- [Sandbox configuration, shared accounts and restarting sessions](sandbox-config.md)
- [Default sessions: console, coordinators and repo homes](default-sessions.md)
- [Codex App Server opt-in and rollback](codex-app-server-supervision.md#opt-in-and-fallback)
- [Remote provider setup and trust model](remote-provider.md)
- [Version compatibility and upgrades](versioning.md)
- [Migrating older isolation/layout schemes](namespace-rollout.md)

## Integrating and developing

- [Contributing and verification commands](../CONTRIBUTING.md)
- [Local daemon and optional legacy relay](client-server.md)
- [Agent CLI access and authorization](agent-cli-sandbox.md)
- [Scoped transcript/event queries](scoped-runtime-events.md)
- [Agent reporting protocol](agent-protocol.md)
- [Provider session, steering and runtime-event protocol](remote-provider-sessions.md)
- [Dated runtime validation results](agent-cli-validation.md)

The root [event-read design](../EVENT_READ_DESIGN.md) and
[dispatch reliability notes](../RELIABILITY_FIX.md) are implementation records.
Their checkpoint language and historical base revisions are not current setup
instructions. The source, CLI help and CI describe the merged implementation;
open PRs may describe behavior that has not shipped yet.

# Protected namespace rollout

The own-only namespace changes apply when a process is created. Rebuilding or
installing an amux binary cannot change mounts, PID visibility, inherited file
descriptors, or the original environment of an already-running pane or App
Server.

## Preconditions

Do not claim session, transcript, process, socket, credential, or Git-object
confidentiality while any legacy pane/AppServer remains alive with the previous
broad data/state/run, sibling-parent, shared-proc, Docker, Windows-drive, history,
or Git-common mounts. A mixture of old and new namespaces has the confidentiality
of the old namespace: it can retain paths and capabilities that no new launch
receives.

A host-authorized rollout must therefore:

1. Inventory every live pane and AppServer, including detached processes.
2. Stop all legacy runtimes before starting protected replacements; do not use a
   rolling mixed-mode restart as confidentiality evidence.
3. Inventory each legacy coordinator root and Git checkout for dirty, staged,
   untracked, ignored-but-owned, rebase/merge, submodule, transcript, private
   configuration, skill, and unknown state.
4. Provision the daemon-owned access roots, dedicated coordinator directory,
   safe host-prepared configuration, and accepted shared-worktree/private-Git
   layout before launch.
5. Refuse recreation when ownership is ambiguous, a source/destination is an
   alias, required state cannot be preserved, or an atomic transition cannot be
   completed. Leave the original filesystem and database state intact for
   explicit host recovery; never discard or silently relocate it from a read.
6. Start the replacements only after the old runtimes are gone, then verify the
   fixed `/amux-session-access` context, `/amux-bin`, the read-only exact-file
   installed-amux alias used by generated Claude hooks/status, private PID/proc
   state, launcher environment, access mounts, and private worktree
   metadata/object grants in the new processes. Verify that the installed
   alias's containing directory and sibling executables remain absent.

These are deployment preconditions, not an instruction to perform host work.
Installation, daemon restart, live-state migration, and recreation require
separate host authorization.

## Deliberate remaining grants

Protected panes still share the host network and receive read-only system roots,
their selected model account, harness auth, Git/GitHub account files, configured
editor/shell resources, the exact own directory, and daemon-authorized Git object
pool generations. Any data readable through those accounts or deliberately
admitted object pools is outside the filesystem-confidentiality claim.

`/amux-bin/amux` grants the exact running executable. Claude configuration also
contains host-compatible absolute hook and status-line commands, so the trusted
launcher binds that same running executable read-only at the stable installed
path. Protected bare and generated absolute commands therefore cannot select
different amux versions, and a missing host installation does not break the
hook. The alias's parent directory and neighboring user-installed tools are not
mounted. Host commands outside protected panes retain the normal installed-path
behavior.

The daemon-private access and socket source parents are mode-restricted and never
mounted. This prevents a restricted session from replacing their path entries
before bubblewrap resolves them; established bind mounts pin the selected inode.
It does not defend against the trusted host operator or a compromised daemon
running under the same host identity.

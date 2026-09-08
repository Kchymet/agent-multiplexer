package git

// GitObjectMount describes one immutable Git object-store generation that an
// agent namespace may read. ObjectsHostDir is the daemon-resolved source;
// ObjectsMountDir is its exact read-only destination inside the namespace.
//
// A repository may require multiple entries. Callers order those entries from
// the oldest retained generation to the tip generation. No path in this
// contract may name a Git common directory: refs, config, hooks, indexes, logs,
// and worktree administration remain private to the session.
//
// This is the shared, frozen boundary between Git provisioning, lifecycle, and
// namespace assembly. Namespace code must validate both paths against trusted
// daemon state and bind only the named object directory read-only.
type GitObjectMount struct {
	RepoKey         string `json:"repo_key"`
	Generation      string `json:"generation"`
	ObjectsHostDir  string `json:"objects_host_dir"`
	ObjectsMountDir string `json:"objects_mount_dir"`
}

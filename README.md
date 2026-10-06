# kube-operator2dir

A Kubernetes operator alternative to [json2dir](https://github.com/alurm/json2dir):
declaratively materialize a JSON-encoded directory tree — files, symlinks,
executable scripts — onto the filesystems of selected cluster nodes, with
full lifecycle control (replace on change, prune on delete, revert external
drift).

Built with [kubebuilder](https://book.kubebuilder.io/) (Go, controller-runtime).
Not a CLI, not a Helm chart — a CRD plus a node-local agent.

## How it works

- The **operator runs as a DaemonSet**, one agent pod per node. Each agent
  mounts the node root filesystem (`hostPath /` at `/host`) and watches
  `DirTree` custom resources (cluster-scoped).
- A `DirTree` CR selects nodes with `spec.nodeSelector` (equality-based
  labels). An agent only manages CRs whose selector matches its own node —
  **a tree never applies to all nodes implicitly**.
- Each agent materializes `spec.tree` under `spec.rootDir` on its node and
  reports its own entry in `status.nodes` (patched per node key, so
  concurrent agents never clobber each other).

### Conversion scheme (json2dir semantics)

```yaml
tree:
  greeting: "Hello, world!"          # regular file
  dir:                               # object -> directory
    subfile: "Content.\n"
    subdir: {}                       # empty directory
  symlink: ["link", "/etc/hostname"] # symlink
  script: ["script", "#!/bin/sh\necho Howdy!"] # executable file
```

Constraints, enforced at validation:

- the tree root must be an object; keys are single path segments
  (no `/`, no `.`, `..`) — nesting comes from nested objects only;
- file content is UTF-8 only (no binary);
- **replace, not merge**: entries absent from the spec are removed;
  `rootDir` is fully owned by the CR, pre-existing content is overwritten.

### Lifecycle semantics

- **Drift enforcement**: every agent re-reconciles on CR changes and on a
  periodic resync (default 5m, `--resync-period`). Files modified outside
  the operator are reverted, based on observed on-disk state.
- **Deletion**: removing the CR prunes the tree on every selected node,
  guarded by the `ops.operator2dir/prune` finalizer. The finalizer is only
  removed once every *live* node matching the selector has reported prune
  completion. Nodes that no longer exist are skipped (no hung deletes); a
  live node whose agent never reports *does* block deletion — strip the
  finalizer manually if that is intended.
- **Re-targeting** (selector label changes): files are left in place when a
  node stops matching; the tree simply stops being enforced there.

## Security model — read before deploying

Writing to node filesystems as root is this operator's core function, so its
blast radius is large by design:

- `spec.rootDir` must be an absolute path and cannot be `/`; the agent
  refuses symlinked roots, never writes through symlinks, replaces
  unexpected entry types (symlink/dir/file) instead of writing into them,
  and writes files atomically (temp file + rename in the target directory).
- Symlink *targets* may point anywhere (json2dir semantics) — creating a
  link never writes through it, and pruning a symlink removes the link, not
  the target.
- Whoever can create `DirTree` CRs can write arbitrary files on the matched
  nodes (root-owned by default, or any uid/gid via `spec.uid`/`spec.gid`).
  Keep the CRD API access restricted accordingly.
- The DaemonSet needs root with `CHOWN`, `DAC_OVERRIDE`, `FOWNER`, `FSETID`
  capabilities. Its RBAC is minimal: read dirtrees, patch status/finalizers,
  read nodes, emit events.

## Try it

```sh
make docker-build docker-push IMG=<registry>/operator2dir:latest
make deploy IMG=<registry>/operator2dir:latest

kubectl label node <node> example.com/dirtree=true
kubectl apply -f config/samples/ops_v1alpha1_dirtree.yaml
kubectl get dirtree dirtree-sample -o yaml   # status.nodes.<node>.phase: Ready
ssh <node> ls -la /etc/operator2dir-example
```

## Development

```sh
make manifests generate   # regenerate CRDs/deepcopy after changing api/
make test                 # envtest-based integration tests (see Makefile)
make install && make run  # run the agent locally against the current kubeconfig
```

See `AGENTS.md` for the repo conventions and the full design constraints.

## License

Apache License 2.0 — see [LICENSE](LICENSE).

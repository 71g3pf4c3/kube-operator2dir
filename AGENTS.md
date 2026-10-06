# AGENTS.md

## Project status

Scaffolded and implemented: kubebuilder v4 layout, `DirTree` CRD (cluster-scoped, `ops.operator2dir/v1alpha1`), node-local agent, envtest suite, deploy manifests. Track `test/e2e` — still scaffold stubs, not real tests.

## Mission

A Kubernetes operator alternative to [json2dir](https://github.com/alurm/json2dir): declaratively materialize a JSON/YAML-defined directory tree (files, symlinks, executable scripts) onto node filesystems, with full lifecycle control. Built with **kubebuilder** (Go, controller-runtime). Not a CLI, not a Helm chart — a CRD + controller.

## Conversion scheme (must match json2dir semantics)

The CR spec's tree field mirrors the json2dir encoding:

- JSON object → directory; object keys → entry names.
- String value → regular file with that content.
- `["link", "<target>"]` → symlink to `<target>`.
- `["script", "<content>"]` → executable file with `<content>`.

Hard semantic constraints inherited from json2dir — enforce at validation, not at write time:

- Tree root must be an object.
- Keys are single path segments: reject keys containing `/`, keys equal to `.`/`..`, and absolute paths. Nested structure comes from nested objects, never from multi-segment keys.
- File content is UTF-8 only; no binary support.
- Overwrite semantics: replace, not merge — a changed tree removes entries that no longer exist in the spec (see Deletion below for CR-level prune).

## Architecture (decided — do not relitigate without asking)

- **DaemonSet-only, no split**: the operator runs as a DaemonSet; each node's agent is a full controller-runtime manager watching DirTree CRs, filtering by its own node's labels (`spec.nodeSelector`, required, `MinProperties=1` — never implicit all-nodes). There is no central Deployment controller. Leader election is off.
- **Write path**: agent mounts hostPath `/` at `/host`; `spec.rootDir` (node-absolute) is resolved under it. FS-writing logic never lives anywhere but the node-local agent.
- **Status**: `status.nodes` map keyed by node name; each agent JSON-merge-patches only its own key, so concurrent agents don't clobber each other. Phases: Ready/Progressing/InvalidSpec/Error/Pruned. Steady-state reconciles patch nothing (change detection in `patchOwnStatus`) — do not "simplify" this into unconditional patches, it would create an infinite watch/patch loop.
- **Prune via finalizer** `ops.operator2dir/prune`: deleting the CR removes the rendered tree. Finalizer is removed only when every *live* node matching the selector reported `pruned: true`; gone nodes are skipped (no hung delete); a live node with no reporting agent blocks deletion by design (manual finalizer strip is the documented escape hatch).
- **Continuous enforcement**: reconcile on watch events (DirTree, own Node label changes) plus periodic resync (`--resync-period`, default 5m). External drift (files modified outside the operator) is reverted on resync. Drift detection compares observed on-disk state, never "we wrote it once".
- **Re-targeting**: when a node stops matching the selector, files stay in place; the tree simply stops being enforced there (documented limitation, not a prune trigger).

## Safety requirements (hard rules — regressions here are security bugs)

- **Path escape is the primary security boundary.** Validation happens twice: CRD schema (root-object, `rootDir` absolute + not `/` via CEL, non-empty selector) and Go (`internal/tree.Parse` + `validateRoot`). `rootDir` must be absolute, cleaned, not `/`, and must not itself be a symlink; every entry name is re-validated as a single safe segment before any write/rename/delete. Never write through a symlink — replace unexpected entry types. json2dir explicitly does not guard TOCTOU; this operator must (the residual lstat→write race is documented in `internal/tree/apply.go`; openat/O_NOFOLLOW traversal is the known future hardening).
- Atomic replace only (temp file + rename in the same directory), so a failed reconcile never leaves half-written files.
- Ownership/permission model is explicit in the CR (`fileMode`/`scriptMode`/`dirMode` octal strings, `uid`/`gid`); unset uid/gid means no chown.
- Agent RBAC is minimal by design: dirtrees get/list/watch, dirtrees/status update/patch, dirtrees/finalizers update, nodes get/list/watch, events create/patch. Leader-election RBAC was removed from `config/rbac` on purpose — don't re-add it.
- The DaemonSet needs root + CHOWN/DAC_OVERRIDE/FOWNER/FSETID caps and mounts hostPath `/` — that is the component's function. Whoever can create DirTree CRs can write anywhere on matching nodes; keep API access restricted accordingly.

## Toolchain / workflow

kubebuilder v4.16 layout: `api/v1alpha1/` (types), `internal/controller/` (agent/reconciler), `internal/tree/` (parse/validate/materialize), `config/` (kustomize manifests). **On this dev box `make` is not in PATH** — run `nix shell nixpkgs#gnumake -c make <target>`.

- `make manifests generate` — regenerate CRDs/deep-copy code. **Run after any change to `api/` types, before build.** Never hand-edit generated `zz_generated.*` or the CRD yaml under `config/crd/bases/`.
- `make install && make run` — run the agent locally against the current kubeconfig.
- `make test` — envtest-based integration tests (verified working; Makefile fetches envtest binaries into `bin/k8s`). Unit tests only: `go test ./internal/tree/...`. The envtest suite must keep a SINGLE manager for the whole suite — controller-runtime registers controller names globally, so a manager per spec fails with "controller with name dirtree already exists".
- `make docker-build docker-push` / `make deploy` — image + DaemonSet deploy.
- `kustomize build config/default` — verify manifests render.

## Gotchas learned the hard way

- `client.MergeFrom(base)` must be captured BEFORE mutating the object, or the computed merge patch is empty and the patch is a silent no-op (bit us in `patchOwnStatus`).
- When removing the finalizer, tolerate NotFound — duplicate reconciles race the CR deletion and erroring there is noise, not a bug.
- In tests, `Expect(os.Stat(...)).NotTo(Succeed())` trips Gomega's multi-return guard — check `os.IsNotExist(err)` explicitly instead.

## Open decisions

- None blocking. Possible future work: admission webhook for full tree validation at CR create time (today deep validation is agent-side, reported via `status.nodes[<node>].phase: InvalidSpec`); openat/O_NOFOLLOW traversal hardening; metrics for drift counters; e2e tests under `test/e2e`.

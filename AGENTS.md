# AGENTS.md

## Project status

Greenfield repo (no scaffold yet). This file is the project charter — update it as the scaffold lands, especially the command names in "Workflow" once a real `Makefile` exists.

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

## Architecture decisions (decided — do not relitigate without asking)

- **hostPath + DaemonSet**: the operator materializes trees onto node filesystems via a DaemonSet component writing to a hostPath. FS-writing logic does NOT live in a single-pod controller — a plain Deployment only sees its own container FS and is the wrong place for this.
- **Prune via finalizer**: deleting the CR removes the rendered tree. Finalizer must be idempotent and must define behavior when the target node is gone (prune skipped + event/condition reported, not a hung delete).
- **Continuous enforcement**: reconcile loop drives observed state → desired state, Puppet-style. External drift (files modified outside the operator) is reverted on resync, not just on spec change. Watch events trigger reconcile; periodic resync (controller-runtime `MaxConcurrentReconciles` / requeue) catches writes that emit no event. Drift detection must be based on observed node state, never on "we wrote it once".

## Safety requirements (agents will get these wrong — these are hard rules)

- **Path escape is the primary security boundary.** A symlink target or a crafted key escaping `rootDir` is arbitrary node-FS write as root (or whoever the agent runs as). Validate tree structure at CR admission (CEL rules on the CRD where feasible) AND re-check at materialize time. Resolve and verify paths stay under the root before any write/rename/delete. json2dir explicitly does not guard TOCTOU; this operator must.
- Prefer atomic replace (write temp file in the same directory + rename) over in-place mutation, so a failed reconcile doesn't leave half-written files.
- hostPath writes need a clearly defined ownership/permission model (uid/gid/mode) in the CR spec; don't invent per-node defaults.
- A tree CR must define which nodes it applies to (node selector / labels). Never assume "all nodes" implicitly.
- DaemonSet agents need minimal RBAC: watch/get the tree CRs, patch status/conditions — nothing broader.

## Toolchain / workflow (post-scaffold)

kubebuilder layout: `api/v1/` (types), `internal/controller/` (reconcilers), `config/` (kustomize manifests), `test/` (envtest). Standard kubebuilder Makefile targets:

- `make manifests generate` — regenerate CRDs/deep-copy code. **Run after any change to `api/` types, before build.** Never hand-edit generated `zz_generated.*` or the CRD yaml.
- `make install && make run` — run the controller locally against the current kubeconfig.
- `make test` — envtest-based integration tests; requires binaries fetched once via `setup-envtest` (see controller-runtime docs). Unit tests that don't need envtest: `go test ./...`.
- `make docker-build docker-push` / `make deploy` — image + deployment.

Verify these target names against the actual `Makefile` after scaffolding and fix this section if they differ.

## Open decisions

- API group/version naming (e.g. `<domain>/v1alpha1`) and the CR kind name.
- Controller/agent split: DaemonSet-only (each agent watches and filters) vs. Deployment controller + DaemonSet agents. Either way, the write path stays node-local.
- Status surface: per-node conditions, drift reporting, last-applied tree hash.

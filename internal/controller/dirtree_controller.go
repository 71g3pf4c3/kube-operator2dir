/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	opsv1alpha1 "github.com/71g3pf4c3/kube-operator2dir/api/v1alpha1"
	"github.com/71g3pf4c3/kube-operator2dir/internal/tree"
)

// FinalizerName is the DirTree finalizer guarding node-local prune.
const FinalizerName = "ops.operator2dir/prune"

// Node phases reported in status.nodes.
const (
	PhaseReady       = "Ready"
	PhaseInvalidSpec = "InvalidSpec"
	PhaseError       = "Error"
	PhasePruned      = "Pruned"
)

// DefaultResyncPeriod is how often a node agent re-reconciles an unchanged
// DirTree to detect and revert external filesystem drift.
const DefaultResyncPeriod = 5 * time.Minute

// DirTreeReconciler runs node-locally (as a DaemonSet agent) and reconciles
// DirTree objects: every agent filters CRs by its own node's labels,
// materializes matching trees under HostRoot+RootDir and reports its own
// per-node status entry.
type DirTreeReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder

	// NodeName is the Kubernetes node this agent runs on.
	NodeName string
	// HostRoot is the mount point of the node filesystem inside the agent
	// container (hostPath / mounted at /host by default).
	HostRoot string
	// ResyncPeriod controls drift-recheck requeueing.
	ResyncPeriod time.Duration
}

// +kubebuilder:rbac:groups=ops.operator2dir,resources=dirtrees,verbs=get;list;watch
// +kubebuilder:rbac:groups=ops.operator2dir,resources=dirtrees/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=ops.operator2dir,resources=dirtrees/finalizers,verbs=update
// +kubebuilder:rbac:groups=core,resources=nodes,verbs=get;list;watch
// +kubebuilder:rbac:groups=core,resources=events,verbs=create;patch

// Reconcile drives the observed state of this node's filesystem towards the
// DirTree spec. All mutating filesystem work happens here, node-locally;
// status is patched per node key only, so concurrent agents do not clobber
// each other's entries.
func (r *DirTreeReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx).WithValues("node", r.NodeName)

	cr := &opsv1alpha1.DirTree{}
	if err := r.Get(ctx, req.NamespacedName, cr); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Own-node view: labels decide targeting.
	node := &corev1.Node{}
	if err := r.Get(ctx, client.ObjectKey{Name: r.NodeName}, node); err != nil {
		return ctrl.Result{}, fmt.Errorf("get own node %s: %w", r.NodeName, err)
	}
	targeted := nodeMatches(node, cr.Spec.NodeSelector)

	// Deletion: prune path. Every agent (targeted or not) participates in
	// the finalizer coverage check.
	if !cr.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, log, cr, targeted)
	}

	// Ensure the finalizer exists so deletion cannot bypass node-local
	// prune. Any agent may add it: it is CR-level and idempotent.
	if !controllerutil.ContainsFinalizer(cr, FinalizerName) {
		patch := client.MergeFrom(cr.DeepCopy())
		controllerutil.AddFinalizer(cr, FinalizerName)
		if err := r.Patch(ctx, cr, patch); err != nil {
			return ctrl.Result{}, fmt.Errorf("add finalizer: %w", err)
		}
	}

	// Not targeted: drop a stale status entry if this node used to be
	// managed. Files are intentionally left in place (relabeling out is not
	// a prune trigger); the tree simply stops being enforced here.
	if !targeted {
		if err := r.dropOwnStatus(ctx, cr); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	// Validate the tree before touching the filesystem.
	parsed, err := tree.Parse(cr.Spec.Tree.Raw)
	if err != nil {
		return r.reportInvalidSpec(ctx, log, cr, err)
	}

	root, err := r.hostRooted(cr.Spec.RootDir)
	if err != nil {
		return r.reportInvalidSpec(ctx, log, cr, err)
	}

	opts, err := treeOptions(cr)
	if err != nil {
		return r.reportInvalidSpec(ctx, log, cr, err)
	}

	changes, err := tree.Apply(root, parsed, opts)
	if err != nil {
		r.Recorder.Event(cr, corev1.EventTypeWarning, "ApplyError",
			fmt.Sprintf("node %s: %v", r.NodeName, err))
		if serr := r.patchOwnStatus(ctx, cr, opsv1alpha1.DirTreeNodeStatus{
			ObservedGeneration: cr.Generation,
			Phase:              PhaseError,
			Message:            err.Error(),
		}); serr != nil {
			return ctrl.Result{}, serr
		}
		// Retry with controller-runtime backoff; the filesystem error may be
		// transient (read-only mount, busy path, ...).
		return ctrl.Result{}, fmt.Errorf("apply on node %s: %w", r.NodeName, err)
	}

	if len(changes) > 0 {
		log.Info("materialized tree", "changes", len(changes), "root", cr.Spec.RootDir)
		r.Recorder.Event(cr, corev1.EventTypeNormal, "Applied",
			fmt.Sprintf("node %s: applied %d change(s)", r.NodeName, len(changes)))
	}

	if err := r.patchOwnStatus(ctx, cr, opsv1alpha1.DirTreeNodeStatus{
		ObservedGeneration: cr.Generation,
		Phase:              PhaseReady,
		LastAppliedHash:    tree.Hash(parsed),
	}); err != nil {
		return ctrl.Result{}, err
	}

	// Continuous enforcement: requeue to catch drift that emits no event.
	return ctrl.Result{RequeueAfter: r.resyncPeriod()}, nil
}

// reconcileDelete prunes the local tree (if this node is targeted) and
// removes the finalizer once every live node matching the selector has
// reported prune completion.
func (r *DirTreeReconciler) reconcileDelete(ctx context.Context, log logr.Logger, cr *opsv1alpha1.DirTree, targeted bool) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(cr, FinalizerName) {
		return ctrl.Result{}, nil
	}

	if targeted {
		root, err := r.hostRooted(cr.Spec.RootDir)
		if err == nil {
			err = tree.Remove(root)
		}
		if err != nil {
			r.Recorder.Event(cr, corev1.EventTypeWarning, "PruneError",
				fmt.Sprintf("node %s: %v", r.NodeName, err))
			if serr := r.patchOwnStatus(ctx, cr, opsv1alpha1.DirTreeNodeStatus{
				ObservedGeneration: cr.Generation,
				Phase:              PhaseError,
				Message:            fmt.Sprintf("prune failed: %v", err),
			}); serr != nil {
				return ctrl.Result{}, serr
			}
			return ctrl.Result{}, fmt.Errorf("prune on node %s: %w", r.NodeName, err)
		}
		if err := r.patchOwnStatus(ctx, cr, opsv1alpha1.DirTreeNodeStatus{
			ObservedGeneration: cr.Generation,
			Phase:              PhasePruned,
			Pruned:             true,
		}); err != nil {
			return ctrl.Result{}, err
		}
		log.Info("pruned local tree", "root", cr.Spec.RootDir)
	}

	// Finalizer coverage: every live node matching the selector must have
	// reported Pruned. Nodes that no longer exist are skipped: their
	// filesystems are gone with the machine, and a hung delete must not
	// result. A live node whose agent never reports (not scheduled, broken)
	// DOES block deletion by design — patch the finalizer away manually if
	// that is intended.
	complete, live, err := r.pruneComplete(ctx, cr)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !complete {
		// Other agents reconcile on the status updates they cause; nothing
		// to do here right now.
		return ctrl.Result{}, nil
	}

	patch := client.MergeFrom(cr.DeepCopy())
	controllerutil.RemoveFinalizer(cr, FinalizerName)
	if err := r.Patch(ctx, cr, patch); err != nil {
		if apierrors.IsNotFound(err) {
			// A concurrent reconcile already removed the finalizer and the
			// CR is gone. Prune is done; nothing left to do.
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("remove finalizer: %w", err)
	}
	r.Recorder.Event(cr, corev1.EventTypeNormal, "Pruned",
		fmt.Sprintf("tree pruned on all %d selected live node(s)", live))
	log.Info("prune complete, removed finalizer")
	return ctrl.Result{}, nil
}

// pruneComplete reports whether every live node matching the selector has
// reported prune completion in status.nodes, along with the number of live
// selected nodes.
func (r *DirTreeReconciler) pruneComplete(ctx context.Context, cr *opsv1alpha1.DirTree) (bool, int, error) {
	nodes := &corev1.NodeList{}
	if err := r.List(ctx, nodes, client.MatchingLabels(cr.Spec.NodeSelector)); err != nil {
		return false, 0, fmt.Errorf("list nodes for prune coverage: %w", err)
	}
	for i := range nodes.Items {
		n := &nodes.Items[i]
		st, ok := cr.Status.Nodes[n.Name]
		if !ok || !st.Pruned {
			return false, len(nodes.Items), nil
		}
	}
	return true, len(nodes.Items), nil
}

// patchOwnStatus merges this node's status entry. Only the agent's own key is
// touched, so concurrent agents writing other keys are not clobbered
// (JSON merge patch on a map merges keys). No-op when nothing relevant
// changed, so steady-state reconciles do not emit watch events.
func (r *DirTreeReconciler) patchOwnStatus(ctx context.Context, cr *opsv1alpha1.DirTree, st opsv1alpha1.DirTreeNodeStatus) error {
	if cr.Status.Nodes == nil {
		cr.Status.Nodes = map[string]opsv1alpha1.DirTreeNodeStatus{}
	}
	cur, ok := cr.Status.Nodes[r.NodeName]
	if ok && statusEqual(cur, st) {
		return nil
	}
	if !ok || cur.Phase != st.Phase {
		st.LastTransitionTime = metav1.Now()
	} else {
		st.LastTransitionTime = cur.LastTransitionTime
	}
	// The merge patch base must be captured BEFORE the mutation, or the
	// computed diff is empty and the patch is a silent no-op.
	base := cr.DeepCopy()
	cr.Status.Nodes[r.NodeName] = st
	return r.Status().Patch(ctx, cr, client.MergeFrom(base))
}

// dropOwnStatus removes this node's status entry (node no longer targeted).
func (r *DirTreeReconciler) dropOwnStatus(ctx context.Context, cr *opsv1alpha1.DirTree) error {
	if _, ok := cr.Status.Nodes[r.NodeName]; !ok {
		return nil
	}
	base := cr.DeepCopy()
	delete(cr.Status.Nodes, r.NodeName)
	return r.Status().Patch(ctx, cr, client.MergeFrom(base))
}

func statusEqual(a, b opsv1alpha1.DirTreeNodeStatus) bool {
	return a.Phase == b.Phase &&
		a.ObservedGeneration == b.ObservedGeneration &&
		a.LastAppliedHash == b.LastAppliedHash &&
		a.Pruned == b.Pruned &&
		a.Message == b.Message
}

// reportInvalidSpec records an InvalidSpec status entry and an event, then
// stops (no requeue: the spec is broken and will not heal by retrying; the
// resync period re-checks anyway).
func (r *DirTreeReconciler) reportInvalidSpec(ctx context.Context, log logr.Logger, cr *opsv1alpha1.DirTree, err error) (ctrl.Result, error) {
	log.Error(err, "invalid DirTree spec")
	r.Recorder.Event(cr, corev1.EventTypeWarning, "InvalidSpec",
		fmt.Sprintf("node %s: %v", r.NodeName, err))
	if serr := r.patchOwnStatus(ctx, cr, opsv1alpha1.DirTreeNodeStatus{
		ObservedGeneration: cr.Generation,
		Phase:              PhaseInvalidSpec,
		Message:            err.Error(),
	}); serr != nil {
		return ctrl.Result{}, serr
	}
	return ctrl.Result{RequeueAfter: r.resyncPeriod()}, nil
}

// hostRooted maps the node-absolute RootDir into the agent container by
// prefixing HostRoot, with the same safety rules as tree.ValidateRoot.
func (r *DirTreeReconciler) hostRooted(rootDir string) (string, error) {
	if !filepath.IsAbs(rootDir) {
		return "", fmt.Errorf("rootDir %q must be absolute", rootDir)
	}
	if filepath.Clean(rootDir) == "/" {
		return "", fmt.Errorf("rootDir must not be the filesystem root")
	}
	return filepath.Join(r.HostRoot, rootDir), nil
}

func (r *DirTreeReconciler) resyncPeriod() time.Duration {
	if r.ResyncPeriod > 0 {
		return r.ResyncPeriod
	}
	return DefaultResyncPeriod
}

// nodeMatches applies the equality-based nodeSelector to a node's labels.
func nodeMatches(node *corev1.Node, selector map[string]string) bool {
	if len(selector) == 0 {
		return false // never implicitly "all nodes"
	}
	return labels.SelectorFromValidatedSet(selector).Matches(labels.Set(node.Labels))
}

// treeOptions converts the CR spec mode/owner fields into tree.Options.
func treeOptions(cr *opsv1alpha1.DirTree) (tree.Options, error) {
	opts := tree.Options{}
	if cr.Spec.FileMode != nil {
		m, err := parseMode(*cr.Spec.FileMode)
		if err != nil {
			return opts, fmt.Errorf("fileMode: %w", err)
		}
		opts.FileMode = m
	}
	if cr.Spec.ScriptMode != nil {
		m, err := parseMode(*cr.Spec.ScriptMode)
		if err != nil {
			return opts, fmt.Errorf("scriptMode: %w", err)
		}
		opts.ScriptMode = m
	}
	if cr.Spec.DirMode != nil {
		m, err := parseMode(*cr.Spec.DirMode)
		if err != nil {
			return opts, fmt.Errorf("dirMode: %w", err)
		}
		opts.DirMode = m
	}
	if cr.Spec.UID != nil {
		u := int(*cr.Spec.UID)
		opts.UID = &u
	}
	if cr.Spec.GID != nil {
		g := int(*cr.Spec.GID)
		opts.GID = &g
	}
	return opts, nil
}

func parseMode(s string) (os.FileMode, error) {
	v, err := strconv.ParseUint(s, 8, 32)
	if err != nil {
		return 0, fmt.Errorf("invalid octal mode %q", s)
	}
	if v > 0o777 {
		return 0, fmt.Errorf("mode %q exceeds 0777", s)
	}
	return os.FileMode(v), nil
}

// SetupWithManager registers the DirTree controller plus a watch on the
// agent's own Node: label changes on our node re-target CRs without waiting
// for the resync period.
func (r *DirTreeReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&opsv1alpha1.DirTree{}).
		Watches(
			&corev1.Node{},
			handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, obj client.Object) []reconcile.Request {
				if obj.GetName() != r.NodeName {
					return nil
				}
				var list opsv1alpha1.DirTreeList
				if err := mgr.GetClient().List(ctx, &list); err != nil {
					return nil
				}
				reqs := make([]reconcile.Request, 0, len(list.Items))
				for i := range list.Items {
					reqs = append(reqs, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&list.Items[i])})
				}
				return reqs
			}),
		).
		Named("dirtree").
		Complete(r)
}

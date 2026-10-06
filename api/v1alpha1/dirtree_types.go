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

package v1alpha1

import (
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// EDIT THIS FILE!  THIS IS SCAFFOLDING FOR YOU TO OWN!
// NOTE: json tags are required.  Any new fields you add must have json tags for the fields to be serialized.

// DirTreeSpec defines the desired state of DirTree.
//
// The CR fully owns rootDir on every selected node: entries not present in
// tree are removed (replace semantics, mirroring json2dir), and pre-existing
// content under rootDir is overwritten. Deleting the CR prunes rootDir on
// every selected node.
type DirTreeSpec struct {
	// rootDir is the absolute path ON THE NODE under which the tree is
	// materialized. The operator DaemonSet mounts the host root filesystem
	// (hostPath / at /host), so this path is resolved relative to that mount.
	//
	// rootDir is fully owned by this CR: any pre-existing content under it is
	// replaced, and pruning (spec changes and CR deletion) removes entries the
	// spec no longer defines.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=2
	// +kubebuilder:validation:Pattern=`^/`
	// +kubebuilder:validation:XValidation:rule="self != '/'",message="rootDir must not be the filesystem root"
	RootDir string `json:"rootDir"`

	// nodeSelector is a label selector (equality-based) picking the nodes this
	// tree applies to. Only nodes whose labels match every key/value pair
	// here are managed. An empty selector is rejected: a tree never applies
	// to all nodes implicitly.
	//
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinProperties=1
	NodeSelector map[string]string `json:"nodeSelector"`

	// tree is the json2dir-encoded directory tree (see
	// https://github.com/alurm/json2dir):
	//
	//   - a JSON object is a directory; its keys are single-segment entry names;
	//   - a string value is a regular file with that content;
	//   - ["link", "<target>"] is a symlink to <target>;
	//   - ["script", "<content>"] is an executable file with <content>.
	//
	// Keys containing "/", the keys "." and "..", and non-object roots are
	// rejected (reported via status per node). File content is UTF-8 only;
	// binary content is not supported.
	//
	// +required
	// +kubebuilder:validation:XPreserveUnknownFields
	// +kubebuilder:validation:Type=object
	Tree apiextensionsv1.JSON `json:"tree"`

	// fileMode is the permission bits (octal string, e.g. "0644") applied to
	// regular files. Defaults to "0644".
	// +optional
	// +kubebuilder:validation:Pattern=`^0[0-7]{3}$`
	FileMode *string `json:"fileMode,omitempty"`

	// scriptMode is the permission bits (octal string) applied to executable
	// files ("script" entries). Defaults to "0755".
	// +optional
	// +kubebuilder:validation:Pattern=`^0[0-7]{3}$`
	ScriptMode *string `json:"scriptMode,omitempty"`

	// dirMode is the permission bits (octal string) applied to directories.
	// Defaults to "0755".
	// +optional
	// +kubebuilder:validation:Pattern=`^0[0-7]{3}$`
	DirMode *string `json:"dirMode,omitempty"`

	// uid is the owner UID applied to all materialized entries. When unset,
	// ownership is left to the creating process (no chown).
	// +optional
	// +kubebuilder:validation:Minimum=0
	UID *int32 `json:"uid,omitempty"`

	// gid is the owner GID applied to all materialized entries. When unset,
	// ownership is left to the creating process (no chown).
	// +optional
	// +kubebuilder:validation:Minimum=0
	GID *int32 `json:"gid,omitempty"`
}

// DirTreeNodeStatus is the observed state of a DirTree on one node, reported
// by that node's agent. Each agent patches only its own entry.
type DirTreeNodeStatus struct {
	// observedGeneration is the metadata.generation most recently reconciled
	// by this node's agent.
	ObservedGeneration int64 `json:"observedGeneration"`

	// phase is this node's reconciliation state:
	// - "Ready": tree materialized and matching the spec;
	// - "Progressing": materialization in progress (reserved);
	// - "InvalidSpec": the tree failed validation (see message);
	// - "Error": materialization failed (see message);
	// - "Pruned": the local tree was removed as part of CR deletion.
	// +kubebuilder:validation:Enum=Ready;Progressing;InvalidSpec;Error;Pruned
	Phase string `json:"phase"`

	// message is a human-readable detail for the current phase.
	// +optional
	Message string `json:"message,omitempty"`

	// lastAppliedHash is a hash of the canonicalized tree last successfully
	// materialized on this node.
	// +optional
	LastAppliedHash string `json:"lastAppliedHash,omitempty"`

	// pruned is true once this node has pruned its local copy of the tree
	// during CR deletion.
	// +optional
	Pruned bool `json:"pruned,omitempty"`

	// lastTransitionTime is the last time phase changed.
	LastTransitionTime metav1.Time `json:"lastTransitionTime"`
}

// DirTreeStatus defines the observed state of DirTree.
type DirTreeStatus struct {
	// nodes holds per-node status, keyed by node name. Each node's agent
	// patches only its own key. Entries for nodes that no longer match
	// nodeSelector are not authoritative and are eventually dropped by the
	// agents.
	//
	// +optional
	// +mapType=granular
	Nodes map[string]DirTreeNodeStatus `json:"nodes,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:storageversion

// DirTree is the Schema for the dirtrees API: it declaratively materializes
// a json2dir-encoded directory tree onto the filesystems of selected nodes.
type DirTree struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of DirTree
	// +required
	Spec DirTreeSpec `json:"spec"`

	// status defines the observed state of DirTree
	// +optional
	Status DirTreeStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// DirTreeList contains a list of DirTree
type DirTreeList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata"`
	Items           []DirTree `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &DirTree{}, &DirTreeList{})
		return nil
	})
}

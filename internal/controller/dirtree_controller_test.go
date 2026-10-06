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
	"os"
	"path/filepath"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	opsv1alpha1 "github.com/71g3pf4c3/kube-operator2dir/api/v1alpha1"
)

func rawJSON(s string) apiextensionsv1.JSON {
	return apiextensionsv1.JSON{Raw: []byte(s)}
}

// The agent (manager, node, hostRoot) is started once in the suite setup;
// these specs only create/clean up DirTree and Node state around it.

var _ = Describe("DirTree agent", func() {
	AfterEach(func() {
		// Clean up test nodes and CRs.
		nodes := &corev1.NodeList{}
		Expect(k8sClient.List(ctx, nodes)).To(Succeed())
		for i := range nodes.Items {
			if nodes.Items[i].Name == agentNodeName {
				continue // owned by the suite
			}
			Expect(k8sClient.Delete(ctx, &nodes.Items[i])).To(Succeed())
		}
		crs := &opsv1alpha1.DirTreeList{}
		Expect(k8sClient.List(ctx, crs)).To(Succeed())
		for i := range crs.Items {
			cr := &crs.Items[i]
			if len(cr.Finalizers) > 0 {
				cr.Finalizers = nil
				Expect(k8sClient.Update(ctx, cr)).To(Succeed())
			}
			Expect(k8sClient.Delete(ctx, cr)).To(Succeed())
		}
	})

	mustCreateTree := func(name, rootDir string, selector map[string]string, tree string) *opsv1alpha1.DirTree {
		cr := &opsv1alpha1.DirTree{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec: opsv1alpha1.DirTreeSpec{
				RootDir:      rootDir,
				NodeSelector: selector,
				Tree:         rawJSON(tree),
			},
		}
		Expect(k8sClient.Create(ctx, cr)).To(Succeed())
		return cr
	}

	getNodeStatus := func(crName string) (opsv1alpha1.DirTreeNodeStatus, bool) {
		cr := &opsv1alpha1.DirTree{}
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: crName}, cr); err != nil {
			return opsv1alpha1.DirTreeNodeStatus{}, false
		}
		st, ok := cr.Status.Nodes[agentNodeName]
		return st, ok
	}

	It("materializes a matching tree and reports Ready", func() {
		cr := mustCreateTree("ready-tree", "/etc/dirtree-test", agentNodeLabels,
			`{"greeting": "Hello, world!", "dir": {"sub": "content"}, "script": ["script", "#!/bin/sh\necho hi"]}`)

		Eventually(func(g Gomega) opsv1alpha1.DirTreeNodeStatus {
			st, ok := getNodeStatus(cr.Name)
			g.Expect(ok).To(BeTrue())
			return st
		}, 10*time.Second, 100*time.Millisecond).Should(And(
			HaveField("Phase", PhaseReady),
			HaveField("ObservedGeneration", cr.Generation),
		))

		localRoot := filepath.Join(hostRoot, "etc", "dirtree-test")
		Expect(os.ReadFile(filepath.Join(localRoot, "greeting"))).To(Equal([]byte("Hello, world!")))
		Expect(os.ReadFile(filepath.Join(localRoot, "dir", "sub"))).To(Equal([]byte("content")))
	})

	It("reverts external drift on resync", func() {
		cr := mustCreateTree("drift-tree", "/etc/dirtree-drift", agentNodeLabels, `{"f": "correct"}`)

		Eventually(func() bool {
			_, ok := getNodeStatus(cr.Name)
			return ok
		}, 10*time.Second, 100*time.Millisecond).Should(BeTrue())

		localFile := filepath.Join(hostRoot, "etc", "dirtree-drift", "f")
		Expect(os.WriteFile(localFile, []byte("tampered"), 0o644)).To(Succeed())

		Eventually(func() string {
			b, err := os.ReadFile(localFile)
			if err != nil {
				return ""
			}
			return string(b)
		}, 10*time.Second, 200*time.Millisecond).Should(Equal("correct"))
	})

	It("reports InvalidSpec for a malformed tree", func() {
		cr := mustCreateTree("invalid-tree", "/etc/dirtree-invalid", agentNodeLabels, `{"a/b": "x"}`)

		Eventually(func(g Gomega) opsv1alpha1.DirTreeNodeStatus {
			st, ok := getNodeStatus(cr.Name)
			g.Expect(ok).To(BeTrue())
			return st
		}, 10*time.Second, 100*time.Millisecond).Should(And(
			HaveField("Phase", PhaseInvalidSpec),
			HaveField("Message", ContainSubstring("single path segment")),
		))
	})

	It("ignores trees that do not target this node", func() {
		mustCreateTree("not-targeted", "/etc/dirtree-ignored",
			map[string]string{"operator2dir-test": "someone-else"}, `{"f": "x"}`)

		Consistently(func() bool {
			_, ok := getNodeStatus("not-targeted")
			return ok
		}, 2*time.Second, 200*time.Millisecond).Should(BeFalse())
	})

	It("prunes the local tree on CR deletion and removes the finalizer", func() {
		cr := mustCreateTree("prune-tree", "/etc/dirtree-prune", agentNodeLabels, `{"f": "x"}`)

		Eventually(func() bool {
			_, ok := getNodeStatus(cr.Name)
			return ok
		}, 10*time.Second, 100*time.Millisecond).Should(BeTrue())
		localRoot := filepath.Join(hostRoot, "etc", "dirtree-prune")
		Expect(os.ReadFile(filepath.Join(localRoot, "f"))).To(Equal([]byte("x")))

		Expect(k8sClient.Delete(ctx, cr)).To(Succeed())

		Eventually(func() bool {
			err := k8sClient.Get(ctx, types.NamespacedName{Name: cr.Name}, &opsv1alpha1.DirTree{})
			return apierrors.IsNotFound(err)
		}, 10*time.Second, 100*time.Millisecond).Should(BeTrue())
		_, err := os.Stat(localRoot)
		Expect(os.IsNotExist(err)).To(BeTrue(), "local tree should be pruned")
	})

	It("blocks deletion while another selected live node has not pruned", func() {
		// Both our node and a second node must match the selector; the
		// second node has no agent running on it.
		agent := &corev1.Node{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: agentNodeName}, agent)).To(Succeed())
		patched := agent.DeepCopy()
		patched.Labels["extra"] = "yes"
		Expect(k8sClient.Patch(ctx, patched, client.MergeFrom(agent))).To(Succeed())

		other := &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name:   "other-node",
				Labels: map[string]string{"operator2dir-test": "agent", "extra": "yes"},
			},
		}
		Expect(k8sClient.Create(ctx, other)).To(Succeed())

		cr := mustCreateTree("blocked-tree", "/etc/dirtree-blocked",
			map[string]string{"operator2dir-test": "agent", "extra": "yes"}, `{"f": "x"}`)

		Eventually(func() bool {
			_, ok := getNodeStatus(cr.Name)
			return ok
		}, 10*time.Second, 100*time.Millisecond).Should(BeTrue())

		Expect(k8sClient.Delete(ctx, cr)).To(Succeed())

		// Our agent pruned, but other-node never reports: the CR must
		// survive with the finalizer, and our status must show Pruned.
		Eventually(func(g Gomega) opsv1alpha1.DirTreeNodeStatus {
			st, ok := getNodeStatus(cr.Name)
			g.Expect(ok).To(BeTrue())
			return st
		}, 10*time.Second, 100*time.Millisecond).Should(HaveField("Pruned", BeTrue()))

		Consistently(func() bool {
			err := k8sClient.Get(ctx, types.NamespacedName{Name: cr.Name}, &opsv1alpha1.DirTree{})
			return err == nil
		}, 2*time.Second, 200*time.Millisecond).Should(BeTrue())

		// The agentless node's files cannot be pruned; the operator out is a
		// manual finalizer strip, verified here.
		fresh := &opsv1alpha1.DirTree{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: cr.Name}, fresh)).To(Succeed())
		fresh.Finalizers = nil
		Expect(k8sClient.Update(ctx, fresh)).To(Succeed())
		Eventually(func() bool {
			err := k8sClient.Get(ctx, types.NamespacedName{Name: cr.Name}, &opsv1alpha1.DirTree{})
			return apierrors.IsNotFound(err)
		}, 10*time.Second, 100*time.Millisecond).Should(BeTrue())
	})

	It("enforces custom ownership model", func() {
		uid := os.Getuid()
		gid := os.Getgid()
		mustCreateTree("owned-tree", "/etc/dirtree-owned", agentNodeLabels, `{"f": "x"}`)

		// The agent races us for the object (finalizer); retry the update.
		Eventually(func(g Gomega) {
			fresh := &opsv1alpha1.DirTree{}
			g.Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "owned-tree"}, fresh)).To(Succeed())
			fresh.Spec.UID = ptr(int32(uid))
			fresh.Spec.GID = ptr(int32(gid))
			fresh.Spec.FileMode = ptr("0600")
			g.Expect(k8sClient.Update(ctx, fresh)).To(Succeed())
		}, 10*time.Second, 500*time.Millisecond).Should(Succeed())

		Eventually(func(g Gomega) opsv1alpha1.DirTreeNodeStatus {
			st, ok := getNodeStatus("owned-tree")
			g.Expect(ok).To(BeTrue())
			return st
		}, 10*time.Second, 100*time.Millisecond).Should(HaveField("Phase", PhaseReady))

		Eventually(func(g Gomega) {
			info, err := os.Lstat(filepath.Join(hostRoot, "etc", "dirtree-owned", "f"))
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(info.Mode().Perm()).To(Equal(os.FileMode(0o600)))
		}, 10*time.Second, 100*time.Millisecond).Should(Succeed())
	})
})

func ptr[T any](v T) *T { return &v }

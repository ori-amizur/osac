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
	"time"

	. "github.com/onsi/ginkgo/v2" //nolint:revive,staticcheck
	. "github.com/onsi/gomega"    //nolint:revive,staticcheck
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/osac-project/osac/osac-operator/api/v1alpha1"
)

var _ = Describe("reconcileAutoExternalIPCleanup", func() {
	const (
		networkingNS = "default"
		clusterUUID  = "test-cluster-uuid"
		pollInterval = 50 * time.Millisecond
	)

	ctx := context.Background()

	newReconciler := func(networkingNamespace string) *ClusterOrderReconciler {
		return &ClusterOrderReconciler{
			Client:              k8sClient,
			NetworkingNamespace: networkingNamespace,
			StatusPollInterval:  pollInterval,
		}
	}

	newClusterOrder := func(uuid string) *v1alpha1.ClusterOrder {
		labels := map[string]string{}
		if uuid != "" {
			labels[osacClusterOrderIDLabel] = uuid
		}
		return &v1alpha1.ClusterOrder{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: "test-co-",
				Namespace:    networkingNS,
				Labels:       labels,
			},
			Spec: v1alpha1.ClusterOrderSpec{TemplateID: "t"},
		}
	}

	newAutoEIA := func(clusterID string) *v1alpha1.ExternalIPAttachment {
		clusterIDCopy := clusterID
		ep := v1alpha1.ExternalIPAttachmentTargetEndpointAPI
		return &v1alpha1.ExternalIPAttachment{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: "test-eia-",
				Namespace:    networkingNS,
				Labels: map[string]string{
					osacAutoProvisionedLabel: labelValueTrue,
				},
			},
			Spec: v1alpha1.ExternalIPAttachmentSpec{
				ExternalIP:     "test-eip",
				Cluster:        &clusterIDCopy,
				TargetEndpoint: &ep,
			},
		}
	}

	newAutoEIP := func(clusterID string) *v1alpha1.ExternalIP {
		return &v1alpha1.ExternalIP{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: "test-eip-",
				Namespace:    networkingNS,
				Labels: map[string]string{
					osacAutoProvisionedLabel:    "true",
					osacAutoProvisionedForLabel: clusterID,
				},
			},
			Spec: v1alpha1.ExternalIPSpec{
				Pool: "test-pool",
			},
		}
	}

	listEIAs := func() []v1alpha1.ExternalIPAttachment {
		list := &v1alpha1.ExternalIPAttachmentList{}
		ExpectWithOffset(1, k8sClient.List(ctx, list, client.InNamespace(networkingNS))).To(Succeed())
		return list.Items
	}

	listEIPs := func() []v1alpha1.ExternalIP {
		list := &v1alpha1.ExternalIPList{}
		ExpectWithOffset(1, k8sClient.List(ctx, list, client.InNamespace(networkingNS))).To(Succeed())
		return list.Items
	}

	cleanupEIAs := func() {
		list := &v1alpha1.ExternalIPAttachmentList{}
		Expect(k8sClient.List(ctx, list, client.InNamespace(networkingNS))).To(Succeed())
		for i := range list.Items {
			_ = k8sClient.Delete(ctx, &list.Items[i])
		}
	}

	cleanupEIPs := func() {
		list := &v1alpha1.ExternalIPList{}
		Expect(k8sClient.List(ctx, list, client.InNamespace(networkingNS))).To(Succeed())
		for i := range list.Items {
			_ = k8sClient.Delete(ctx, &list.Items[i])
		}
	}

	AfterEach(func() {
		cleanupEIAs()
		cleanupEIPs()
	})

	Context("no-op cases", func() {
		It("returns done=true when NetworkingNamespace is empty", func() {
			r := newReconciler("")
			co := newClusterOrder(clusterUUID)
			done, result, err := r.reconcileAutoExternalIPCleanup(ctx, co)
			Expect(err).NotTo(HaveOccurred())
			Expect(done).To(BeTrue())
			Expect(result.RequeueAfter).To(BeZero())
		})

		It("returns done=true when ClusterOrder has no UUID label", func() {
			r := newReconciler(networkingNS)
			co := newClusterOrder("")
			done, result, err := r.reconcileAutoExternalIPCleanup(ctx, co)
			Expect(err).NotTo(HaveOccurred())
			Expect(done).To(BeTrue())
			Expect(result.RequeueAfter).To(BeZero())
		})

		It("returns done=true when no auto-provisioned resources exist", func() {
			r := newReconciler(networkingNS)
			co := newClusterOrder(clusterUUID)
			done, result, err := r.reconcileAutoExternalIPCleanup(ctx, co)
			Expect(err).NotTo(HaveOccurred())
			Expect(done).To(BeTrue())
			Expect(result.RequeueAfter).To(BeZero())
		})
	})

	Context("ExternalIPAttachment cleanup", func() {
		It("deletes auto-provisioned EIAs targeting this cluster and requeues", func() {
			eia := newAutoEIA(clusterUUID)
			Expect(k8sClient.Create(ctx, eia)).To(Succeed())

			r := newReconciler(networkingNS)
			co := newClusterOrder(clusterUUID)
			done, result, err := r.reconcileAutoExternalIPCleanup(ctx, co)
			Expect(err).NotTo(HaveOccurred())
			Expect(done).To(BeFalse())
			Expect(result.RequeueAfter).To(Equal(pollInterval))

			// EIA should have a deletion timestamp or be gone
			remaining := listEIAs()
			for _, e := range remaining {
				Expect(e.DeletionTimestamp).NotTo(BeNil())
			}
		})

		It("does not delete EIAs targeting a different cluster", func() {
			otherEIA := newAutoEIA("different-uuid")
			Expect(k8sClient.Create(ctx, otherEIA)).To(Succeed())

			r := newReconciler(networkingNS)
			co := newClusterOrder(clusterUUID)
			done, result, err := r.reconcileAutoExternalIPCleanup(ctx, co)
			Expect(err).NotTo(HaveOccurred())
			Expect(done).To(BeTrue())
			Expect(result.RequeueAfter).To(BeZero())

			// Other cluster's EIA must not be deleted
			remaining := listEIAs()
			Expect(remaining).To(HaveLen(1))
			Expect(remaining[0].DeletionTimestamp).To(BeNil())
		})

		It("does not delete EIAs without the auto-provisioned label", func() {
			clusterIDCopy := clusterUUID
			ep := v1alpha1.ExternalIPAttachmentTargetEndpointAPI
			manualEIA := &v1alpha1.ExternalIPAttachment{
				ObjectMeta: metav1.ObjectMeta{
					GenerateName: "manual-eia-",
					Namespace:    networkingNS,
				},
				Spec: v1alpha1.ExternalIPAttachmentSpec{
					ExternalIP:     "test-eip",
					Cluster:        &clusterIDCopy,
					TargetEndpoint: &ep,
				},
			}
			Expect(k8sClient.Create(ctx, manualEIA)).To(Succeed())

			r := newReconciler(networkingNS)
			co := newClusterOrder(clusterUUID)
			done, result, err := r.reconcileAutoExternalIPCleanup(ctx, co)
			Expect(err).NotTo(HaveOccurred())
			Expect(done).To(BeTrue())
			Expect(result.RequeueAfter).To(BeZero())

			remaining := listEIAs()
			Expect(remaining).To(HaveLen(1))
			Expect(remaining[0].DeletionTimestamp).To(BeNil())
		})
	})

	Context("ExternalIP cleanup", func() {
		It("deletes auto-provisioned EIPs for this cluster after EIAs are gone and requeues", func() {
			eip := newAutoEIP(clusterUUID)
			Expect(k8sClient.Create(ctx, eip)).To(Succeed())

			r := newReconciler(networkingNS)
			co := newClusterOrder(clusterUUID)
			done, result, err := r.reconcileAutoExternalIPCleanup(ctx, co)
			Expect(err).NotTo(HaveOccurred())
			Expect(done).To(BeFalse())
			Expect(result.RequeueAfter).To(Equal(pollInterval))

			remaining := listEIPs()
			for _, e := range remaining {
				Expect(e.DeletionTimestamp).NotTo(BeNil())
			}
		})

		It("does not delete EIPs labeled for a different cluster", func() {
			eip := newAutoEIP("different-uuid")
			Expect(k8sClient.Create(ctx, eip)).To(Succeed())

			r := newReconciler(networkingNS)
			co := newClusterOrder(clusterUUID)
			done, result, err := r.reconcileAutoExternalIPCleanup(ctx, co)
			Expect(err).NotTo(HaveOccurred())
			Expect(done).To(BeTrue())
			Expect(result.RequeueAfter).To(BeZero())

			remaining := listEIPs()
			Expect(remaining).To(HaveLen(1))
			Expect(remaining[0].DeletionTimestamp).To(BeNil())
		})
	})

	Context("phased ordering", func() {
		It("waits for EIAs to be gone before deleting EIPs", func() {
			eia := newAutoEIA(clusterUUID)
			Expect(k8sClient.Create(ctx, eia)).To(Succeed())
			eip := newAutoEIP(clusterUUID)
			Expect(k8sClient.Create(ctx, eip)).To(Succeed())

			r := newReconciler(networkingNS)
			co := newClusterOrder(clusterUUID)

			// First call: EIA phase — EIA gets deleted, EIP untouched
			done, result, err := r.reconcileAutoExternalIPCleanup(ctx, co)
			Expect(err).NotTo(HaveOccurred())
			Expect(done).To(BeFalse())
			Expect(result.RequeueAfter).To(Equal(pollInterval))

			// EIP must not be deleted yet
			eipList := listEIPs()
			Expect(eipList).To(HaveLen(1))
			Expect(eipList[0].DeletionTimestamp).To(BeNil())
		})

		It("returns done=true when both EIAs and EIPs are gone", func() {
			r := newReconciler(networkingNS)
			co := newClusterOrder(clusterUUID)
			done, result, err := r.reconcileAutoExternalIPCleanup(ctx, co)
			Expect(err).NotTo(HaveOccurred())
			Expect(done).To(BeTrue())
			Expect(result.RequeueAfter).To(BeZero())
		})
	})
})

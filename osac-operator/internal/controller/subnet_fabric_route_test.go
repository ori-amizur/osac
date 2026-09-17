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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	osacv1alpha1 "github.com/osac-project/osac/osac-operator/api/v1alpha1"
	"github.com/osac-project/osac/osac-operator/pkg/dispatcher"
	"github.com/osac-project/osac/osac-operator/pkg/provisioning"
)

var _ = Describe("Subnet fabric route reconciliation", func() {
	It("requires a fabric route only for Kubernetes-owned Secondary Subnets", func() {
		subnet := &osacv1alpha1.Subnet{
			ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{
				osacNetworkingTypeAnnotation: string(osacv1alpha1.VirtualNetworkNetworkingTypeSecondary),
			}},
			Spec: osacv1alpha1.SubnetSpec{IPv4CIDR: "10.20.1.0/24"},
		}
		vnet := &osacv1alpha1.VirtualNetwork{ObjectMeta: metav1.ObjectMeta{
			Annotations: map[string]string{
				osacTransitCapabilityAnnotation:      transitCapabilityNetrisEVPN,
				osacImplementationStrategyAnnotation: netrisFabricManagerName,
			},
		}}

		Expect(fabricRouteRequired(subnet, vnet, string(dispatcher.ManagerRoleK8s))).To(BeTrue())
		Expect(fabricRouteRequired(subnet, vnet, string(dispatcher.ManagerRoleFabric))).To(BeFalse())

		vnet.Annotations[osacTransitCapabilityAnnotation] = transitCapabilityNone
		Expect(fabricRouteRequired(subnet, vnet, string(dispatcher.ManagerRoleK8s))).To(BeFalse())

		vnet.Annotations[osacTransitCapabilityAnnotation] = transitCapabilityAgentlessVLANLocalNet
		Expect(fabricRouteRequired(subnet, vnet, string(dispatcher.ManagerRoleK8s))).To(BeTrue())
	})

	It("dispatches a route-only operation after the Kubernetes Subnet owner succeeds", func() {
		ctx := context.Background()
		routeProvider := &mockSubnetProvider{}
		var triggeredResource client.Object
		routeProvider.triggerProvisionFunc = func(_ context.Context, resource client.Object) (*provisioning.ProvisionResult, error) {
			triggeredResource = resource
			return &provisioning.ProvisionResult{
				JobID:        "fabric-route-job",
				InitialState: osacv1alpha1.JobStatePending,
				Message:      "route job triggered",
			}, nil
		}

		subnet := &osacv1alpha1.Subnet{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "fabric-route-subnet",
				Namespace: "default",
				Annotations: map[string]string{
					osacNetworkingTypeAnnotation: string(osacv1alpha1.VirtualNetworkNetworkingTypeSecondary),
				},
			},
			Spec: osacv1alpha1.SubnetSpec{
				VirtualNetwork: "vn-uuid",
				IPv4CIDR:       "10.20.1.0/24",
			},
			Status: osacv1alpha1.SubnetStatus{
				DesiredConfigVersion: "owner-version",
				ProvisioningJobs: []osacv1alpha1.JobStatus{{
					JobID:         "k8s-subnet-job",
					Type:          osacv1alpha1.JobTypeProvision,
					State:         osacv1alpha1.JobStateSucceeded,
					ConfigVersion: "owner-version",
					Target:        string(dispatcher.ManagerRoleK8s),
				}},
			},
		}
		Expect(k8sClient.Create(ctx, subnet)).To(Succeed())
		// The API server ignores status on create; restore the owner job in the
		// in-memory object used by this direct lifecycle test.
		subnet.Status = osacv1alpha1.SubnetStatus{
			DesiredConfigVersion: "owner-version",
			ProvisioningJobs: []osacv1alpha1.JobStatus{{
				JobID:         "k8s-subnet-job",
				Type:          osacv1alpha1.JobTypeProvision,
				State:         osacv1alpha1.JobStateSucceeded,
				ConfigVersion: "owner-version",
				Target:        string(dispatcher.ManagerRoleK8s),
			}},
		}
		DeferCleanup(func() {
			Expect(k8sClient.Delete(ctx, subnet)).To(Succeed())
		})

		vnet := &osacv1alpha1.VirtualNetwork{ObjectMeta: metav1.ObjectMeta{
			Annotations: map[string]string{
				osacTransitCapabilityAnnotation:      transitCapabilityNetrisEVPN,
				osacImplementationStrategyAnnotation: netrisFabricManagerName,
			},
		}}
		Expect(subnet.Annotations).To(HaveKeyWithValue(osacNetworkingTypeAnnotation, string(osacv1alpha1.VirtualNetworkNetworkingTypeSecondary)))
		Expect(vnet.Annotations).To(HaveKeyWithValue(osacTransitCapabilityAnnotation, transitCapabilityNetrisEVPN))
		Expect(fabricRouteRequired(subnet, vnet, string(dispatcher.ManagerRoleK8s))).To(BeTrue())
		Expect(allProvisionTargetsSucceeded(subnet.Status.ProvisioningJobs, subnet.Status.DesiredConfigVersion, string(dispatcher.ManagerRoleK8s))).To(BeTrue())
		reconciler := &SubnetReconciler{
			Client:                     k8sClient,
			APIReader:                  k8sClient,
			FabricRouteProvider:        routeProvider,
			MaxJobHistory:              10,
			StatusPollInterval:         time.Second,
			NetworkProvisioningEnabled: true,
		}

		_, err := reconciler.handleFabricRouteProvisioning(ctx, subnet, vnet, string(dispatcher.ManagerRoleK8s))
		Expect(err).NotTo(HaveOccurred())
		Expect(triggeredResource).NotTo(BeNil())
		Expect(triggeredResource.GetAnnotations()).To(HaveKeyWithValue(osacImplementationStrategyAnnotation, netrisFabricManagerName))
		Expect(subnet.Status.FabricRouteJobs).To(HaveLen(1))
		Expect(subnet.Status.FabricRouteJobs[0].Target).To(Equal(string(dispatcher.ManagerRoleFabric)))
		Expect(subnet.Status.FabricRouteJobs[0].JobID).To(Equal("fabric-route-job"))
	})

	It("uses the persisted route manager when withdrawing a route", func() {
		ctx := context.Background()
		routeProvider := &mockSubnetProvider{}
		var deprovisionedResource client.Object
		routeProvider.triggerDeprovisionFunc = func(_ context.Context, resource client.Object, _ []osacv1alpha1.JobStatus) (*provisioning.DeprovisionResult, error) {
			deprovisionedResource = resource
			return &provisioning.DeprovisionResult{
				Action:                 provisioning.DeprovisionTriggered,
				JobID:                  "agentless-route-delete-job",
				BlockDeletionOnFailure: true,
			}, nil
		}

		subnet := &osacv1alpha1.Subnet{
			ObjectMeta: metav1.ObjectMeta{Name: "persisted-route-manager-subnet", Namespace: "default"},
			Status: osacv1alpha1.SubnetStatus{
				FabricRouteImplementationStrategy: "agentless_net",
				FabricRouteJobs: []osacv1alpha1.JobStatus{{
					JobID:     "agentless-route-create-job",
					Type:      osacv1alpha1.JobTypeProvision,
					State:     osacv1alpha1.JobStateSucceeded,
					Target:    string(dispatcher.ManagerRoleFabric),
					Timestamp: metav1.Now(),
				}},
			},
		}
		Expect(k8sClient.Create(ctx, subnet)).To(Succeed())
		subnet.Status = osacv1alpha1.SubnetStatus{
			FabricRouteImplementationStrategy: "agentless_net",
			FabricRouteJobs: []osacv1alpha1.JobStatus{{
				JobID:     "agentless-route-create-job",
				Type:      osacv1alpha1.JobTypeProvision,
				State:     osacv1alpha1.JobStateSucceeded,
				Target:    string(dispatcher.ManagerRoleFabric),
				Timestamp: metav1.Now(),
			}},
		}
		DeferCleanup(func() {
			Expect(k8sClient.Delete(ctx, subnet)).To(Succeed())
		})
		Expect(provisioning.FindLatestJobByTypeAndTarget(subnet.Status.FabricRouteJobs, osacv1alpha1.JobTypeProvision, string(dispatcher.ManagerRoleFabric))).NotTo(BeNil())

		reconciler := &SubnetReconciler{
			Client:              k8sClient,
			FabricRouteProvider: routeProvider,
			MaxJobHistory:       10,
			StatusPollInterval:  time.Second,
		}

		_, err := reconciler.handleFabricRouteDeprovisioning(ctx, subnet)
		Expect(err).NotTo(HaveOccurred())
		Expect(deprovisionedResource).NotTo(BeNil())
		Expect(deprovisionedResource.GetAnnotations()).To(HaveKeyWithValue(osacImplementationStrategyAnnotation, "agentless_net"))
	})
})

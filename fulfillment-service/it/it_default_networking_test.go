/*
Copyright (c) 2026 Red Hat Inc.

Licensed under the Apache License, Version 2.0 (the "License"); you may not use this file except in compliance with the
License. You may obtain a copy of the License at

  http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software distributed under the License is distributed on an
"AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied. See the License for the specific
language governing permissions and limitations under the License.
*/

package it

import (
	"context"
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2/dsl/core"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"

	privatev1 "github.com/osac-project/osac/fulfillment-service/internal/api/osac/private/v1"
	"github.com/osac-project/osac/fulfillment-service/internal/kubernetes/labels"
	"github.com/osac-project/osac/fulfillment-service/internal/uuid"
	osacv1alpha1 "github.com/osac-project/osac/osac-operator/api/v1alpha1"
)

var _ = Describe("Default networking provisioning", func() {
	var (
		ctx context.Context

		tenantsClient        privatev1.TenantsClient
		networkClassesClient privatev1.NetworkClassesClient
		virtualNetworksClient privatev1.VirtualNetworksClient
		subnetsClient        privatev1.SubnetsClient
		securityGroupsClient privatev1.SecurityGroupsClient

		networkClassId string
	)

	BeforeEach(func() {
		ctx = context.Background()

		tenantsClient = privatev1.NewTenantsClient(tool.InternalView().AdminConn())
		networkClassesClient = privatev1.NewNetworkClassesClient(tool.InternalView().AdminConn())
		virtualNetworksClient = privatev1.NewVirtualNetworksClient(tool.InternalView().AdminConn())
		subnetsClient = privatev1.NewSubnetsClient(tool.InternalView().AdminConn())
		securityGroupsClient = privatev1.NewSecurityGroupsClient(tool.InternalView().AdminConn())

		// Create a default NetworkClass with defaults so ensureDefaultNetworking fires.
		ncResp, err := networkClassesClient.Create(ctx, privatev1.NetworkClassesCreateRequest_builder{
			Object: privatev1.NetworkClass_builder{
				Metadata:               privatev1.Metadata_builder{Name: fmt.Sprintf("test-default-nc-%s", uuid.New())}.Build(),
				Title:                  "Test Default Network Class",
				ImplementationStrategy: "cudn_net",
				FabricManager:          new("cudn_net"),
				IsDefault:              new(true),
				Spec: privatev1.NetworkClassSpec_builder{
					Defaults: privatev1.NetworkDefaults_builder{
						VirtualNetworkIpv4Cidr: "10.200.0.0/16",
						SubnetIpv4Cidr:         "10.200.0.0/20",
					}.Build(),
				}.Build(),
			}.Build(),
		}.Build())
		Expect(err).ToNot(HaveOccurred())
		networkClassId = ncResp.GetObject().GetId()
		DeferCleanup(func() {
			_, _ = networkClassesClient.Delete(ctx, privatev1.NetworkClassesDeleteRequest_builder{
				Id: networkClassId,
			}.Build())
		})
	})

	It("creates K8s CRs for default VN/Subnet/SG and transitions DefaultNetworkingReady to True", func(ctx context.Context) {
		tenantName := fmt.Sprintf("test-defnet-%s", uuid.New())

		By("Creating tenant and waiting for SYNCED")
		tenantId := createTenant(ctx, tenantsClient, tenantName)
		waitForTenantSynced(ctx, tenantsClient, tenantId)

		defaultLabelFilter := fmt.Sprintf(
			"this.metadata.labels['osac.openshift.io/default'] == 'true' && this.metadata.tenant == %q",
			tenantName,
		)

		By("Waiting for DefaultNetworkingReady=False/ResourcesPending (ensureDefaultNetworking ran)")
		Eventually(func(g Gomega) {
			resp, err := tenantsClient.Get(ctx, privatev1.TenantsGetRequest_builder{Id: tenantId}.Build())
			g.Expect(err).ToNot(HaveOccurred())
			cond := findTenantCondition(resp.GetObject().GetStatus().GetConditions(),
				privatev1.TenantConditionType_TENANT_CONDITION_TYPE_DEFAULT_NETWORKING_READY)
			g.Expect(cond).ToNot(BeNil())
			g.Expect(cond.GetStatus()).To(Equal(privatev1.ConditionStatus_CONDITION_STATUS_FALSE))
			g.Expect(cond.HasReason()).To(BeTrue())
			g.Expect(cond.GetReason()).To(Equal("ResourcesPending"))
		}, time.Minute, time.Second).Should(Succeed())

		By("Waiting for default VirtualNetwork to appear in FS DB")
		var vnId string
		Eventually(func(g Gomega) {
			resp, err := virtualNetworksClient.List(ctx, privatev1.VirtualNetworksListRequest_builder{
				Filter: &defaultLabelFilter,
			}.Build())
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(resp.GetItems()).ToNot(BeEmpty())
			vnId = resp.GetItems()[0].GetId()
		}, time.Minute, time.Second).Should(Succeed())

		By("Checking at least one hub is registered (VN reconciler selectHub requires a hub)")
		hubsClient := privatev1.NewHubsClient(tool.InternalView().AdminConn())
		hubsResp, err := hubsClient.List(ctx, privatev1.HubsListRequest_builder{}.Build())
		Expect(err).ToNot(HaveOccurred())
		Expect(hubsResp.GetItems()).ToNot(BeEmpty(),
			"No hubs registered — VN reconciler selectHub will always fail")

		By("Control: creating an external VN for the same tenant via private API (same NC, same tenant, different creation path)")
		extVnResp, err := virtualNetworksClient.Create(ctx, privatev1.VirtualNetworksCreateRequest_builder{
			Object: privatev1.VirtualNetwork_builder{
				Metadata: privatev1.Metadata_builder{
					Name:   "external-vn",
					Tenant: tenantName,
				}.Build(),
				Spec: privatev1.VirtualNetworkSpec_builder{
					NetworkClass: privatev1.NetworkClassReference_builder{
						Id: networkClassId,
					}.Build(),
					Ipv4Cidr: new("10.201.0.0/16"),
					Region:   "default",
				}.Build(),
			}.Build(),
		}.Build())
		Expect(err).ToNot(HaveOccurred())
		extVnId := extVnResp.GetObject().GetId()
		DeferCleanup(func() {
			virtualNetworksClient.Delete(ctx, privatev1.VirtualNetworksDeleteRequest_builder{Id: extVnId}.Build())
		})

		By("Control step 1: external VN gets finalizer (VN reconciler processes external creations)")
		Eventually(func(g Gomega) {
			resp, err := virtualNetworksClient.Get(ctx, privatev1.VirtualNetworksGetRequest_builder{Id: extVnId}.Build())
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(resp.GetObject().GetMetadata().GetFinalizers()).To(
				ContainElement("fulfillment-controller"),
				"VN reconciler did not process externally-created VN — broader reconciler issue",
			)
		}, 30*time.Second, time.Second).Should(Succeed())

		// ── Comprehensive diagnostics ────────────────────────────────────────────────
		// We know (from prior runs) that:
		//   • Control VN (external creation, no default label) → finalizer set in ~1s ✓
		//   • Default VN (ensureDefaultNetworking, has default label) → finalizer never set ✗
		//   • Signal() on default VN → finalizer still never set after 60s ✗
		//
		// Goal: determine whether the VN reconciler (a) never receives the event, or
		// (b) receives it but Update() fails. We also want to know what the VN looks
		// like in the DB (state, hub, message) to identify the failure mode.

		By("Diagnostic: verify the default VN is readable by the test (basic connectivity)")
		preSignalVn, err := virtualNetworksClient.Get(ctx, privatev1.VirtualNetworksGetRequest_builder{Id: vnId}.Build())
		Expect(err).ToNot(HaveOccurred(),
			"Failed to fetch default VN from FS — connectivity or visibility issue")
		print(fmt.Sprintf("[diag] default VN pre-signal: state=%v hub=%q finalizers=%v message=%q creator=%q\n",
			preSignalVn.GetObject().GetStatus().GetState(),
			preSignalVn.GetObject().GetStatus().GetHub(),
			preSignalVn.GetObject().GetMetadata().GetFinalizers(),
			preSignalVn.GetObject().GetStatus().GetMessage(),
			preSignalVn.GetObject().GetMetadata().GetCreator(),
		))

		By("Diagnostic: verify external (control) VN is readable — establishes that VNs in this tenant are accessible")
		extPreSignalVn, extErr := virtualNetworksClient.Get(ctx, privatev1.VirtualNetworksGetRequest_builder{Id: extVnId}.Build())
		Expect(extErr).ToNot(HaveOccurred())
		print(fmt.Sprintf("[diag] control VN pre-signal: state=%v hub=%q finalizers=%v\n",
			extPreSignalVn.GetObject().GetStatus().GetState(),
			extPreSignalVn.GetObject().GetStatus().GetHub(),
			extPreSignalVn.GetObject().GetMetadata().GetFinalizers(),
		))

		By("Diagnostic step 1b: Signal() the default VN, Subnet, and SG — forces all three reconcilers to re-process")
		// Signal fires a SIGNALED event, waking each reconciler. The Subnet/SG events from
		// ensureDefaultNetworking suffer the same delivery issue as the VN event.
		_, err = virtualNetworksClient.Signal(ctx, privatev1.VirtualNetworksSignalRequest_builder{Id: vnId}.Build())
		Expect(err).ToNot(HaveOccurred())
		// Also signal Subnet and SG once their IDs are known
		var subnetIdEarly, sgIdEarly string
		{
			f := fmt.Sprintf("this.metadata.labels['osac.openshift.io/default'] == 'true' && this.metadata.tenant == %q", tenantName)
			if sResp, sErr := subnetsClient.List(ctx, privatev1.SubnetsListRequest_builder{Filter: &f}.Build()); sErr == nil && len(sResp.GetItems()) > 0 {
				subnetIdEarly = sResp.GetItems()[0].GetId()
				subnetsClient.Signal(ctx, privatev1.SubnetsSignalRequest_builder{Id: subnetIdEarly}.Build())
				print(fmt.Sprintf("[diag] signaled subnet=%s\n", subnetIdEarly))
			}
			if sgResp, sgErr := securityGroupsClient.List(ctx, privatev1.SecurityGroupsListRequest_builder{Filter: &f}.Build()); sgErr == nil && len(sgResp.GetItems()) > 0 {
				sgIdEarly = sgResp.GetItems()[0].GetId()
				securityGroupsClient.Signal(ctx, privatev1.SecurityGroupsSignalRequest_builder{Id: sgIdEarly}.Build())
				print(fmt.Sprintf("[diag] signaled sg=%s\n", sgIdEarly))
			}
		}

		By("Diagnostic step 1: poll default VN state every 5s for 60s and log all changes after Signal")
		// At each poll we log: state, hub, finalizers, message.
		// - If nothing changes → Signal event not reaching the reconciler (still delivery bug).
		// - If state becomes PENDING but no finalizer → impossible (selectHub requires finalizer first).
		// - If finalizer appears → reconciler is working now; CREATE event was the issue.
		// After 60s we fail with the full history to identify the exact stuck point.
		var lastState, lastHub string
		var lastFinalizers []string
		for i := range 12 { // 12 × 5s = 60s
			time.Sleep(5 * time.Second)
			resp, getErr := virtualNetworksClient.Get(ctx, privatev1.VirtualNetworksGetRequest_builder{Id: vnId}.Build())
			if getErr != nil {
				print(fmt.Sprintf("[diag] poll %d: Get failed: %v\n", i, getErr))
				continue
			}
			vn := resp.GetObject()
			state := fmt.Sprintf("%v", vn.GetStatus().GetState())
			hub := vn.GetStatus().GetHub()
			finals := vn.GetMetadata().GetFinalizers()
			msg := vn.GetStatus().GetMessage()
			if state != lastState || hub != lastHub || fmt.Sprintf("%v", finals) != fmt.Sprintf("%v", lastFinalizers) {
				print(fmt.Sprintf("[diag] poll %d (t+%ds): state=%s hub=%q finalizers=%v message=%q\n",
					i, (i+1)*5, state, hub, finals, msg))
				lastState, lastHub, lastFinalizers = state, hub, finals
			}
			hasFinalizer := false
			for _, f := range finals {
				if f == "fulfillment-controller" {
					hasFinalizer = true
				}
			}
			if hasFinalizer {
				break
			}
		}
		// Final state snapshot and assertion:
		finalResp, finalErr := virtualNetworksClient.Get(ctx, privatev1.VirtualNetworksGetRequest_builder{Id: vnId}.Build())
		Expect(finalErr).ToNot(HaveOccurred())
		finalVn := finalResp.GetObject()
		print(fmt.Sprintf("[diag] default VN final state: state=%v hub=%q finalizers=%v message=%q\n",
			finalVn.GetStatus().GetState(),
			finalVn.GetStatus().GetHub(),
			finalVn.GetMetadata().GetFinalizers(),
			finalVn.GetStatus().GetMessage(),
		))
		Expect(finalVn.GetMetadata().GetFinalizers()).To(
			ContainElement("fulfillment-controller"),
			fmt.Sprintf(
				"finalizer not set 60s after Signal() — VN reconciler is either not receiving the event or Update() is failing; "+
					"final state: state=%v hub=%q finalizers=%v message=%q; "+
					"pre-signal state: state=%v hub=%q finalizers=%v",
				finalVn.GetStatus().GetState(), finalVn.GetStatus().GetHub(),
				finalVn.GetMetadata().GetFinalizers(), finalVn.GetStatus().GetMessage(),
				preSignalVn.GetObject().GetStatus().GetState(), preSignalVn.GetObject().GetStatus().GetHub(),
				preSignalVn.GetObject().GetMetadata().GetFinalizers(),
			),
		)

		By("Diagnostic step 2: VN reconciler selects hub (confirms second reconciliation pass completed)")
		Eventually(func(g Gomega) {
			resp, err := virtualNetworksClient.Get(ctx, privatev1.VirtualNetworksGetRequest_builder{Id: vnId}.Build())
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(resp.GetObject().GetStatus().GetHub()).ToNot(BeEmpty(),
				"VN reconciler hub not set — reconciler ran once (finalizer) but stalled before selectHub",
			)
		}, 30*time.Second, time.Second).Should(Succeed())

		By("Diagnostic step 3: VN reconciler creates K8s CR (confirms third reconciliation pass completed)")
		kubeClient := tool.KubeClient()
		vnList := &osacv1alpha1.VirtualNetworkList{}
		Eventually(func(g Gomega) {
			err := kubeClient.List(ctx, vnList, crclient.MatchingLabels{
				labels.VirtualNetworkUuid: vnId,
			})
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(vnList.Items).To(HaveLen(1),
				"VN K8s CR not created — reconciler set hub but stalled before K8s creation",
			)
		}, time.Minute, time.Second).Should(Succeed())

		By("Waiting for default VirtualNetwork to reach PENDING state before overriding")
		Eventually(func(g Gomega) {
			resp, err := virtualNetworksClient.Get(ctx, privatev1.VirtualNetworksGetRequest_builder{Id: vnId}.Build())
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(resp.GetObject().GetStatus().GetState()).To(
				Equal(privatev1.VirtualNetworkState_VIRTUAL_NETWORK_STATE_PENDING))
		}, time.Minute, time.Second).Should(Succeed())

		By("Setting default VirtualNetwork to READY (no osac-operator in IT)")
		vnResp, err := virtualNetworksClient.Get(ctx, privatev1.VirtualNetworksGetRequest_builder{Id: vnId}.Build())
		Expect(err).ToNot(HaveOccurred())
		vnObj := vnResp.GetObject()
		vnObj.SetStatus(privatev1.VirtualNetworkStatus_builder{
			State: privatev1.VirtualNetworkState_VIRTUAL_NETWORK_STATE_READY,
		}.Build())
		_, err = virtualNetworksClient.Update(ctx, privatev1.VirtualNetworksUpdateRequest_builder{
			Object:     vnObj,
			UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"status.state"}},
		}.Build())
		Expect(err).ToNot(HaveOccurred())

		By("Waiting for default Subnet to appear in FS DB")
		var subnetId string
		subnetFilter := fmt.Sprintf(
			"this.metadata.labels['osac.openshift.io/default'] == 'true' && this.metadata.tenant == %q",
			tenantName,
		)
		Eventually(func(g Gomega) {
			resp, err := subnetsClient.List(ctx, privatev1.SubnetsListRequest_builder{
				Filter: &subnetFilter,
			}.Build())
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(resp.GetItems()).ToNot(BeEmpty())
			subnetId = resp.GetItems()[0].GetId()
		}, time.Minute, time.Second).Should(Succeed())

		By("Verifying default Subnet K8s CR is created")
		subnetList := &osacv1alpha1.SubnetList{}
		Eventually(func(g Gomega) {
			err := kubeClient.List(ctx, subnetList, crclient.MatchingLabels{
				labels.SubnetUuid: subnetId,
			})
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(subnetList.Items).To(HaveLen(1))
		}, time.Minute, time.Second).Should(Succeed())

		By("Waiting for default Subnet to reach PENDING state before overriding")
		Eventually(func(g Gomega) {
			resp, err := subnetsClient.Get(ctx, privatev1.SubnetsGetRequest_builder{Id: subnetId}.Build())
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(resp.GetObject().GetStatus().GetState()).To(
				Equal(privatev1.SubnetState_SUBNET_STATE_PENDING))
		}, time.Minute, time.Second).Should(Succeed())

		By("Setting default Subnet to READY")
		subResp, err := subnetsClient.Get(ctx, privatev1.SubnetsGetRequest_builder{Id: subnetId}.Build())
		Expect(err).ToNot(HaveOccurred())
		subObj := subResp.GetObject()
		subObj.SetStatus(privatev1.SubnetStatus_builder{
			State: privatev1.SubnetState_SUBNET_STATE_READY,
		}.Build())
		_, err = subnetsClient.Update(ctx, privatev1.SubnetsUpdateRequest_builder{
			Object:     subObj,
			UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"status.state"}},
		}.Build())
		Expect(err).ToNot(HaveOccurred())

		By("Waiting for default SecurityGroup to appear in FS DB")
		var sgId string
		sgFilter := fmt.Sprintf(
			"this.metadata.labels['osac.openshift.io/default'] == 'true' && this.metadata.tenant == %q",
			tenantName,
		)
		Eventually(func(g Gomega) {
			resp, err := securityGroupsClient.List(ctx, privatev1.SecurityGroupsListRequest_builder{
				Filter: &sgFilter,
			}.Build())
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(resp.GetItems()).ToNot(BeEmpty())
			sgId = resp.GetItems()[0].GetId()
		}, time.Minute, time.Second).Should(Succeed())

		By("Verifying default SecurityGroup K8s CR is created")
		sgList := &osacv1alpha1.SecurityGroupList{}
		Eventually(func(g Gomega) {
			err := kubeClient.List(ctx, sgList, crclient.MatchingLabels{
				labels.SecurityGroupUuid: sgId,
			})
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(sgList.Items).To(HaveLen(1))
		}, time.Minute, time.Second).Should(Succeed())

		By("Waiting for default SecurityGroup to reach PENDING state before overriding")
		Eventually(func(g Gomega) {
			resp, err := securityGroupsClient.Get(ctx, privatev1.SecurityGroupsGetRequest_builder{Id: sgId}.Build())
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(resp.GetObject().GetStatus().GetState()).To(
				Equal(privatev1.SecurityGroupState_SECURITY_GROUP_STATE_PENDING))
		}, time.Minute, time.Second).Should(Succeed())

		By("Setting default SecurityGroup to READY")
		sgResp, err := securityGroupsClient.Get(ctx, privatev1.SecurityGroupsGetRequest_builder{Id: sgId}.Build())
		Expect(err).ToNot(HaveOccurred())
		sgObj := sgResp.GetObject()
		sgObj.SetStatus(privatev1.SecurityGroupStatus_builder{
			State: privatev1.SecurityGroupState_SECURITY_GROUP_STATE_READY,
		}.Build())
		_, err = securityGroupsClient.Update(ctx, privatev1.SecurityGroupsUpdateRequest_builder{
			Object:     sgObj,
			UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"status.state"}},
		}.Build())
		Expect(err).ToNot(HaveOccurred())

		By("Waiting for DefaultNetworkingReady=True/AllResourcesReady")
		Eventually(func(g Gomega) {
			resp, err := tenantsClient.Get(ctx, privatev1.TenantsGetRequest_builder{Id: tenantId}.Build())
			g.Expect(err).ToNot(HaveOccurred())
			cond := findTenantCondition(resp.GetObject().GetStatus().GetConditions(),
				privatev1.TenantConditionType_TENANT_CONDITION_TYPE_DEFAULT_NETWORKING_READY)
			g.Expect(cond).ToNot(BeNil())
			g.Expect(cond.GetStatus()).To(Equal(privatev1.ConditionStatus_CONDITION_STATUS_TRUE))
			g.Expect(cond.HasReason()).To(BeTrue())
			g.Expect(cond.GetReason()).To(Equal("AllResourcesReady"))
		}, time.Minute, time.Second).Should(Succeed())
	})

	It("sets DefaultNetworkingReady=True/NoDefaultNetworking when no default NetworkClass has defaults", func(ctx context.Context) {
		// First remove the default NetworkClass created in BeforeEach so that
		// no default NetworkClass with defaults is present.
		_, err := networkClassesClient.Delete(ctx, privatev1.NetworkClassesDeleteRequest_builder{
			Id: networkClassId,
		}.Build())
		Expect(err).ToNot(HaveOccurred())
		networkClassId = "" // prevent double-delete in DeferCleanup

		tenantName := fmt.Sprintf("test-nodefnet-%s", uuid.New())

		By("Creating tenant and waiting for SYNCED")
		tenantId := createTenant(ctx, tenantsClient, tenantName)
		waitForTenantSynced(ctx, tenantsClient, tenantId)

		By("Waiting for DefaultNetworkingReady=True/NoDefaultNetworking")
		Eventually(func(g Gomega) {
			resp, err := tenantsClient.Get(ctx, privatev1.TenantsGetRequest_builder{Id: tenantId}.Build())
			g.Expect(err).ToNot(HaveOccurred())
			cond := findTenantCondition(resp.GetObject().GetStatus().GetConditions(),
				privatev1.TenantConditionType_TENANT_CONDITION_TYPE_DEFAULT_NETWORKING_READY)
			g.Expect(cond).ToNot(BeNil())
			g.Expect(cond.GetStatus()).To(Equal(privatev1.ConditionStatus_CONDITION_STATUS_TRUE))
			g.Expect(cond.HasReason()).To(BeTrue())
			g.Expect(cond.GetReason()).To(Equal("NoDefaultNetworking"))
		}, time.Minute, time.Second).Should(Succeed())
	})

})

func findTenantCondition(conditions []*privatev1.TenantCondition, condType privatev1.TenantConditionType) *privatev1.TenantCondition {
	for _, c := range conditions {
		if c.GetType() == condType {
			return c
		}
	}
	return nil
}

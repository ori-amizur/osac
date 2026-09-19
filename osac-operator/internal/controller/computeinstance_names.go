/*
Copyright 2025.

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
	"fmt"
)

const (
	defaultComputeInstanceNamespace string = "osac-computeinstance"

	computeInstanceControllerName = "computeinstance-controller"

	eventReasonTenantNotReady      = "TenantNotReady"
	eventReasonProvisioningStorage = "ProvisioningStorage"
	eventReasonInfrastructureReady = "InfrastructureReady"
	eventReasonProvisioningFailed  = "ProvisioningFailed"
	eventReasonReady               = "Ready"

	eventActionReconcile = "Reconcile"
)

var (
	osacComputeInstanceNameLabel                 string = fmt.Sprintf("%s/computeinstance", osacPrefix)
	osacComputeInstanceIDLabel                   string = fmt.Sprintf("%s/computeinstance-uuid", osacPrefix)
	osacComputeInstanceFinalizer                 string = fmt.Sprintf("%s/computeinstance", osacPrefix)
	osacComputeInstanceFeedbackFinalizer         string = fmt.Sprintf("%s/computeinstance-feedback", osacPrefix)
	osacComputeInstanceManagementStateAnnotation string = fmt.Sprintf("%s/management-state", osacPrefix)
	osacSubnetTargetNamespaceAnnotation          string = fmt.Sprintf("%s/subnet-target-namespace", osacPrefix)
	// osacCUDNReconcileAnnotation is updated after OSAC adds a secondary-subnet
	// selector label to a VM namespace. The metadata change makes
	// OVN-Kubernetes reconcile the CUDN and create the namespace-local NAD.
	osacCUDNReconcileAnnotation string = fmt.Sprintf("%s/cudn-reconcile-at", osacPrefix)
	// osacSecondarySubnetLabelsSyncedAnnotation marks that syncSecondarySubnetLabels has
	// already run for this ComputeInstance. NetworkAttachments are immutable, so the derived
	// secondary-subnet.osac.openshift.io/<uuid> label set never changes after creation.
	osacSecondarySubnetLabelsSyncedAnnotation string = fmt.Sprintf("%s/secondary-subnet-labels-synced", osacPrefix)
)

// secondarySubnetLabelPrefix labels a ComputeInstance (and, mirrored, its target namespace)
// with each Secondary Subnet it attaches to. The router namespace carries the same label for
// every Subnet in the VirtualNetwork. This lets a Subnet CUDN select the router namespace and
// only the VM namespaces that actually use that Subnet, without exposing the transit Primary
// CUDN to VM namespaces.
const secondarySubnetLabelPrefix = "secondary-subnet.osac.openshift.io/"

// legacySecondaryVNLabelPrefix identifies the pre-subnet-scoped selector labels.
// They are removed from VM instances and their target namespaces during one-time
// metadata migration; the VN/router namespace intentionally keeps its VN label
// because the transit CUDN still uses it.
const legacySecondaryVNLabelPrefix = "secondary-vn.osac.openshift.io/"

// secondarySubnetLabelKey returns the label key used to mark a dependency on the Subnet
// identified by subnetID.
func secondarySubnetLabelKey(subnetID string) string {
	return secondarySubnetLabelPrefix + subnetID
}

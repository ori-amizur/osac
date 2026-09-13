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

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// VirtualNetworkSpec defines the desired state of VirtualNetwork
type VirtualNetworkSpec struct {
	// Region is the cloud region where this VirtualNetwork will be provisioned
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Type=string
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="region is immutable"
	Region string `json:"region"`

	// IPv4CIDR is the IPv4 CIDR block for this virtual network
	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:Type=string
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="ipv4Cidr is immutable"
	IPv4CIDR string `json:"ipv4Cidr,omitempty"`

	// IPv6CIDR is the IPv6 CIDR block for this virtual network
	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:Type=string
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="ipv6Cidr is immutable"
	IPv6CIDR string `json:"ipv6Cidr,omitempty"`

	// NetworkClass is the name of the NetworkClass that defines implementation strategy.
	// When omitted, the platform default NetworkClass is used.
	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:Type=string
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="networkClass is immutable"
	NetworkClass string `json:"networkClass,omitempty"`

	// NetworkingType selects the VirtualNetwork connectivity model. When omitted, defaults to Primary.
	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:Enum=Primary;Secondary
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="networkingType is immutable"
	NetworkingType VirtualNetworkNetworkingType `json:"networkingType,omitempty"`
}

// VirtualNetworkNetworkingType is a valid value for .spec.networkingType
// +kubebuilder:validation:Enum=Primary;Secondary
type VirtualNetworkNetworkingType string

const (
	// VirtualNetworkNetworkingTypePrimary is OSAC's existing VirtualNetwork networking model.
	VirtualNetworkNetworkingTypePrimary VirtualNetworkNetworkingType = "Primary"

	// VirtualNetworkNetworkingTypeSecondary is the router-pod / Secondary UDN networking model.
	VirtualNetworkNetworkingTypeSecondary VirtualNetworkNetworkingType = "Secondary"
)

// VirtualNetworkPhaseType is a valid value for .status.phase
// +kubebuilder:validation:Enum=Progressing;Ready;Failed;Deleting
type VirtualNetworkPhaseType string

const (
	// VirtualNetworkPhaseProgressing means an update is in progress
	VirtualNetworkPhaseProgressing VirtualNetworkPhaseType = "Progressing"

	// VirtualNetworkPhaseReady means the virtual network and all associated resources are ready
	VirtualNetworkPhaseReady VirtualNetworkPhaseType = "Ready"

	// VirtualNetworkPhaseFailed means the virtual network provisioning has failed
	VirtualNetworkPhaseFailed VirtualNetworkPhaseType = "Failed"

	// VirtualNetworkPhaseDeleting means there has been a request to delete the VirtualNetwork
	VirtualNetworkPhaseDeleting VirtualNetworkPhaseType = "Deleting"
)

// RouterPodRecoveryStatus records the latest router-pod recovery attempt for a
// Secondary VirtualNetwork. PodUID is the idempotency key: duplicate Pod events
// for the same replacement must not launch another active recovery job.
type RouterPodRecoveryStatus struct {
	// PodUID identifies the router Pod whose networking is being reconciled.
	// +kubebuilder:validation:Optional
	PodUID string `json:"podUID,omitempty"`

	// JobID is the AAP recovery job identifier.
	// +kubebuilder:validation:Optional
	JobID string `json:"jobID,omitempty"`

	// State is the latest recovery job state.
	// +kubebuilder:validation:Optional
	State JobState `json:"state,omitempty"`

	// Attempt counts recovery attempts for this Pod UID.
	// +kubebuilder:validation:Optional
	Attempt int32 `json:"attempt,omitempty"`

	// Message contains the latest provider or reconciliation message.
	// +kubebuilder:validation:Optional
	Message string `json:"message,omitempty"`

	// LastAttemptTime is when the latest recovery attempt was started.
	// +kubebuilder:validation:Optional
	LastAttemptTime *metav1.Time `json:"lastAttemptTime,omitempty"`

	// NextRetryTime prevents a failed recovery from being retried in a tight loop.
	// +kubebuilder:validation:Optional
	NextRetryTime *metav1.Time `json:"nextRetryTime,omitempty"`
}

// VirtualNetworkStatus defines the observed state of VirtualNetwork
type VirtualNetworkStatus struct {
	// Phase provides a single-value overview of the state of the VirtualNetwork
	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:Type=string
	// +kubebuilder:validation:Enum=Progressing;Ready;Failed;Deleting
	Phase VirtualNetworkPhaseType `json:"phase,omitempty"`

	// DesiredConfigVersion is a hash of the spec, used to detect spec changes and control retry behavior.
	// +kubebuilder:validation:Optional
	DesiredConfigVersion string `json:"desiredConfigVersion,omitempty"`

	// ProvisioningJobs holds an array of JobStatus tracking provisioning and deprovisioning operations
	// +kubebuilder:validation:Optional
	ProvisioningJobs []JobStatus `json:"provisioningJobs,omitempty"`

	// BackendNetworkID stores provider-specific network identifier
	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:Type=string
	BackendNetworkID string `json:"backendNetworkId,omitempty"`

	// RouterPodRecovery records recovery of the current Secondary VirtualNetwork
	// router Pod, independently of the normal VirtualNetwork provisioning job.
	// +kubebuilder:validation:Optional
	RouterPodRecovery *RouterPodRecoveryStatus `json:"routerPodRecovery,omitempty"`

	// Conditions holds an array of metav1.Condition that describe the state of the VirtualNetwork
	// +kubebuilder:validation:Optional
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type" protobuf:"bytes,1,rep,name=conditions"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=vnet
// +kubebuilder:printcolumn:name="NetworkClass",type=string,JSONPath=`.spec.networkClass`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// VirtualNetwork is the Schema for the virtualnetworks API
type VirtualNetwork struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitempty,omitzero"`

	// spec defines the desired state of VirtualNetwork
	// +required
	Spec VirtualNetworkSpec `json:"spec"`

	// status defines the observed state of VirtualNetwork
	// +optional
	Status VirtualNetworkStatus `json:"status,omitempty,omitzero"`
}

// +kubebuilder:object:root=true

// VirtualNetworkList contains a list of VirtualNetwork
type VirtualNetworkList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []VirtualNetwork `json:"items"`
}

// GetName returns the name of the VirtualNetwork resource
func (v *VirtualNetwork) GetName() string {
	return v.ObjectMeta.Name
}

package controller

import (
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/osac-project/osac/osac-operator/api/v1alpha1"
	"github.com/osac-project/osac/osac-operator/pkg/dispatcher"
)

// reconcileParentRouterReadiness gates Subnet Ready on the parent Secondary
// VirtualNetwork's latest router recovery. The Subnet's own AAP job can remain
// successfully applied while a replacement router Pod is still converging.
func (r *SubnetReconciler) reconcileParentRouterReadiness(
	subnet *v1alpha1.Subnet,
	vnet *v1alpha1.VirtualNetwork,
	plan *dispatcher.DispatchPlan,
) ctrl.Result {
	if vnet.Spec.NetworkingType != v1alpha1.VirtualNetworkNetworkingTypeSecondary {
		return ctrl.Result{}
	}
	// A provider that does not implement router recovery has no recovery gate.
	// The VN controller creates this status only when the recovery contract is
	// active, which preserves existing behavior for other providers and tests.
	if vnet.Status.RouterPodRecovery == nil {
		return ctrl.Result{}
	}

	configApplied := isSubnetConfigApplied(subnet.Status.ProvisioningJobs, subnet.Status.DesiredConfigVersion, plan)
	recoveryReady := vnet.Status.RouterPodRecovery != nil &&
		vnet.Status.RouterPodRecovery.State == v1alpha1.JobStateSucceeded &&
		vnet.Status.RouterPodRecovery.PodUID != ""
	if !recoveryReady {
		if configApplied || subnet.Status.Phase == v1alpha1.SubnetPhaseReady {
			subnet.Status.Phase = v1alpha1.SubnetPhaseProgressing
			setReadyConditionBlocked(&subnet.Status.Conditions, routerPodRecoveryPendingReason,
				"waiting for the parent router Pod recovery to complete")
		}
		return ctrl.Result{RequeueAfter: r.StatusPollInterval}
	}

	if configApplied {
		subnet.Status.Phase = v1alpha1.SubnetPhaseReady
		setReadyConditionTrue(&subnet.Status.Conditions)
	}
	return ctrl.Result{}
}

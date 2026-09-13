package controller

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	mc "sigs.k8s.io/multicluster-runtime/pkg/multicluster"
	mcreconcile "sigs.k8s.io/multicluster-runtime/pkg/reconcile"

	"github.com/osac-project/osac/osac-operator/api/v1alpha1"
	"github.com/osac-project/osac/osac-operator/pkg/provisioning"
)

const (
	osacRouterPodLabel             = osacPrefix + "/router-pod"
	osacRouterVirtualNetworkLabel  = osacPrefix + "/virtual-network"
	routerPodRecoveryConditionType = "RouterPodRecoveryReady"
	routerPodRecoveryPendingReason = "RouterPodRecoveryPending"
	routerPodRecoveryRunningReason = "RouterPodRecoveryRunning"
	routerPodRecoveryFailedReason  = "RouterPodRecoveryFailed"
	routerPodRecoveryReadyReason   = "RouterPodRecoverySucceeded"

	routerPodRecoveryMaxBackoff = 5 * time.Minute
)

// reconcileRouterPodRecovery observes the current target-cluster router Pod and
// drives a separate AAP repair job. It deliberately does not use the normal VN
// config-version lifecycle: a Pod replacement does not change the VN spec.
func (r *VirtualNetworkReconciler) reconcileRouterPodRecovery(
	ctx context.Context,
	vnet *v1alpha1.VirtualNetwork,
	baseReady bool,
) (ctrlResult ctrl.Result, err error) {
	if vnet.Spec.NetworkingType != v1alpha1.VirtualNetworkNetworkingTypeSecondary {
		return ctrl.Result{}, nil
	}

	recoveryProvider, ok := r.ProvisioningProvider.(provisioning.RouterPodRecoveryProvider)
	if !ok {
		// Providers without a router-pod implementation do not own this recovery
		// contract. This keeps Primary and non-CUDN backends unchanged.
		return ctrl.Result{}, nil
	}

	targetClient, err := r.routerPodTargetClient(ctx)
	if err != nil {
		return ctrl.Result{}, err
	}

	currentPod, err := r.currentReadyRouterPod(ctx, targetClient, vnet)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return r.markRouterPodRecoveryWaiting(vnet, baseReady, "router Pod is not present"), nil
		}
		return ctrl.Result{}, err
	}
	if currentPod == nil {
		return r.markRouterPodRecoveryWaiting(vnet, baseReady, "waiting for a Ready replacement router Pod"), nil
	}

	if vnet.Status.RouterPodRecovery == nil || vnet.Status.RouterPodRecovery.PodUID != string(currentPod.UID) {
		vnet.Status.RouterPodRecovery = &v1alpha1.RouterPodRecoveryStatus{
			PodUID:  string(currentPod.UID),
			State:   v1alpha1.JobStatePending,
			Message: "replacement router Pod is Ready; recovery is pending",
		}
	}

	recovery := vnet.Status.RouterPodRecovery
	if recovery.State == v1alpha1.JobStateSucceeded {
		setRouterPodRecoveryCondition(vnet, metav1.ConditionTrue, routerPodRecoveryReadyReason, recovery.Message)
		if baseReady {
			vnet.Status.Phase = v1alpha1.VirtualNetworkPhaseReady
			setReadyConditionTrue(&vnet.Status.Conditions)
		}
		return ctrl.Result{}, nil
	}

	setRouterPodRecoveryCondition(vnet, metav1.ConditionFalse, routerPodRecoveryPendingReason, recovery.Message)
	vnet.Status.Phase = v1alpha1.VirtualNetworkPhaseProgressing
	setReadyConditionBlocked(&vnet.Status.Conditions, routerPodRecoveryPendingReason, recovery.Message)

	if recovery.JobID != "" && !recovery.State.IsTerminal() {
		return r.pollRouterPodRecovery(ctx, vnet, recovery, baseReady), nil
	}

	if recovery.State == v1alpha1.JobStateFailed && recovery.NextRetryTime != nil && time.Now().Before(recovery.NextRetryTime.Time) {
		return ctrl.Result{RequeueAfter: time.Until(recovery.NextRetryTime.Time)}, nil
	}

	recovery.Attempt++
	recovery.LastAttemptTime = ptrToTime(time.Now().UTC())
	recovery.NextRetryTime = nil
	recovery.State = v1alpha1.JobStatePending
	recovery.Message = fmt.Sprintf("starting router Pod recovery attempt %d", recovery.Attempt)

	result, triggerErr := recoveryProvider.TriggerRouterPodRecovery(ctx, vnet, recovery.PodUID)
	if triggerErr != nil {
		r.markRouterPodRecoveryFailure(recovery, triggerErr.Error())
		r.recordRouterPodRecoveryEvent(vnet, corev1.EventTypeWarning, routerPodRecoveryFailedReason, "Recovery", recovery.Message)
		setRouterPodRecoveryCondition(vnet, metav1.ConditionFalse, routerPodRecoveryFailedReason, recovery.Message)
		setReadyConditionBlocked(&vnet.Status.Conditions, routerPodRecoveryFailedReason, recovery.Message)
		return ctrl.Result{RequeueAfter: recoveryBackoff(recovery.Attempt)}, nil
	}
	if result == nil || result.JobID == "" {
		message := "router Pod recovery provider returned no job ID"
		r.markRouterPodRecoveryFailure(recovery, message)
		r.recordRouterPodRecoveryEvent(vnet, corev1.EventTypeWarning, routerPodRecoveryFailedReason, "Recovery", message)
		setRouterPodRecoveryCondition(vnet, metav1.ConditionFalse, routerPodRecoveryFailedReason, message)
		setReadyConditionBlocked(&vnet.Status.Conditions, routerPodRecoveryFailedReason, message)
		return ctrl.Result{RequeueAfter: recoveryBackoff(recovery.Attempt)}, nil
	}

	recovery.JobID = result.JobID
	recovery.State = result.InitialState
	recovery.Message = result.Message
	action := "Recovery"
	if recovery.Attempt > 1 {
		action = "Retry"
	}
	r.recordRouterPodRecoveryEvent(vnet, corev1.EventTypeNormal, routerPodRecoveryRunningReason, action, recovery.Message)
	setRouterPodRecoveryCondition(vnet, metav1.ConditionFalse, routerPodRecoveryRunningReason, recovery.Message)
	setReadyConditionBlocked(&vnet.Status.Conditions, routerPodRecoveryRunningReason, recovery.Message)
	return ctrl.Result{RequeueAfter: r.StatusPollInterval}, nil
}

// pollRouterPodRecovery advances the persisted recovery job state. A status
// lookup failure is retried without starting another AAP job, which prevents
// transient AAP API failures from violating the one-active-job-per-Pod rule.
func (r *VirtualNetworkReconciler) pollRouterPodRecovery(
	ctx context.Context,
	vnet *v1alpha1.VirtualNetwork,
	recovery *v1alpha1.RouterPodRecoveryStatus,
	baseReady bool,
) ctrl.Result {
	status, err := r.ProvisioningProvider.GetProvisionStatus(ctx, vnet, recovery.JobID)
	if err != nil {
		recovery.Message = fmt.Sprintf("failed to read recovery job %s: %v", recovery.JobID, err)
		return ctrl.Result{RequeueAfter: r.StatusPollInterval}
	}

	recovery.State = status.State
	recovery.Message = status.MessageWithDetails()
	if !status.State.IsTerminal() {
		setRouterPodRecoveryCondition(vnet, metav1.ConditionFalse, routerPodRecoveryRunningReason, recovery.Message)
		setReadyConditionBlocked(&vnet.Status.Conditions, routerPodRecoveryRunningReason, recovery.Message)
		return ctrl.Result{RequeueAfter: r.StatusPollInterval}
	}

	if status.State.IsSuccessful() {
		recovery.NextRetryTime = nil
		r.recordRouterPodRecoveryEvent(vnet, corev1.EventTypeNormal, routerPodRecoveryReadyReason, "Recovery", recovery.Message)
		setRouterPodRecoveryCondition(vnet, metav1.ConditionTrue, routerPodRecoveryReadyReason, recovery.Message)
		if baseReady {
			vnet.Status.Phase = v1alpha1.VirtualNetworkPhaseReady
			setReadyConditionTrue(&vnet.Status.Conditions)
		}
		return ctrl.Result{}
	}

	r.markRouterPodRecoveryFailure(recovery, recovery.Message)
	r.recordRouterPodRecoveryEvent(vnet, corev1.EventTypeWarning, routerPodRecoveryFailedReason, "Recovery", recovery.Message)
	setRouterPodRecoveryCondition(vnet, metav1.ConditionFalse, routerPodRecoveryFailedReason, recovery.Message)
	setReadyConditionBlocked(&vnet.Status.Conditions, routerPodRecoveryFailedReason, recovery.Message)
	return ctrl.Result{RequeueAfter: recoveryBackoff(recovery.Attempt)}
}

func (r *VirtualNetworkReconciler) routerPodTargetClient(ctx context.Context) (client.Client, error) {
	if r.targetCluster == "" {
		return r.Client, nil
	}
	return getTargetClient(ctx, r.mgr, r.targetCluster)
}

func (r *VirtualNetworkReconciler) currentReadyRouterPod(
	ctx context.Context,
	targetClient client.Client,
	vnet *v1alpha1.VirtualNetwork,
) (*corev1.Pod, error) {
	pods := &corev1.PodList{}
	if err := targetClient.List(ctx, pods,
		client.InNamespace(vnet.Name),
		client.MatchingLabels{
			osacRouterPodLabel:            labelValueTrue,
			osacRouterVirtualNetworkLabel: vnet.Name,
		},
	); err != nil {
		return nil, err
	}

	var ready []*corev1.Pod
	for i := range pods.Items {
		pod := &pods.Items[i]
		if pod.DeletionTimestamp != nil || pod.Status.Phase != corev1.PodRunning || !podReady(pod) {
			continue
		}
		ready = append(ready, pod)
	}
	if len(ready) > 1 {
		return nil, fmt.Errorf("found %d Ready router Pods for VirtualNetwork %s", len(ready), vnet.Name)
	}
	if len(ready) == 0 {
		return nil, nil
	}
	return ready[0], nil
}

func (r *VirtualNetworkReconciler) markRouterPodRecoveryWaiting(
	vnet *v1alpha1.VirtualNetwork,
	baseReady bool,
	message string,
) ctrl.Result {
	if vnet.Status.RouterPodRecovery == nil {
		vnet.Status.RouterPodRecovery = &v1alpha1.RouterPodRecoveryStatus{State: v1alpha1.JobStatePending}
	}
	// A previously recovered Pod is no longer authoritative once it disappears
	// or becomes unready. Clear the active job so a later replacement UID gets a
	// fresh, exactly-once recovery attempt.
	vnet.Status.RouterPodRecovery.PodUID = ""
	vnet.Status.RouterPodRecovery.JobID = ""
	vnet.Status.RouterPodRecovery.State = v1alpha1.JobStatePending
	vnet.Status.RouterPodRecovery.NextRetryTime = nil
	vnet.Status.RouterPodRecovery.Message = message
	setRouterPodRecoveryCondition(vnet, metav1.ConditionFalse, routerPodRecoveryPendingReason, message)
	if baseReady {
		vnet.Status.Phase = v1alpha1.VirtualNetworkPhaseProgressing
		setReadyConditionBlocked(&vnet.Status.Conditions, routerPodRecoveryPendingReason, message)
	}
	return ctrl.Result{RequeueAfter: r.StatusPollInterval}
}

func (r *VirtualNetworkReconciler) markRouterPodRecoveryFailure(
	recovery *v1alpha1.RouterPodRecoveryStatus,
	message string,
) {
	recovery.State = v1alpha1.JobStateFailed
	recovery.Message = message
	recovery.JobID = ""
	next := time.Now().UTC().Add(recoveryBackoff(recovery.Attempt))
	recovery.NextRetryTime = &metav1.Time{Time: next}
}

func setRouterPodRecoveryCondition(vnet *v1alpha1.VirtualNetwork, status metav1.ConditionStatus, reason, message string) {
	apimeta.SetStatusCondition(&vnet.Status.Conditions, metav1.Condition{
		Type:    routerPodRecoveryConditionType,
		Status:  status,
		Reason:  reason,
		Message: message,
	})
}

func (r *VirtualNetworkReconciler) recordRouterPodRecoveryEvent(
	vnet *v1alpha1.VirtualNetwork,
	eventType, reason, action, message string,
) {
	if r.Recorder == nil {
		return
	}
	r.Recorder.Eventf(vnet, nil, eventType, reason, action, "%s", message)
}

func recoveryBackoff(attempt int32) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	backoff := 15 * time.Second
	for i := int32(1); i < attempt && backoff < routerPodRecoveryMaxBackoff; i++ {
		backoff *= 2
	}
	if backoff > routerPodRecoveryMaxBackoff {
		return routerPodRecoveryMaxBackoff
	}
	return backoff
}

func podReady(pod *corev1.Pod) bool {
	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodReady {
			return condition.Status == corev1.ConditionTrue
		}
	}
	return false
}

func ptrToTime(value time.Time) *metav1.Time {
	return &metav1.Time{Time: value}
}

// mapRouterPodToVirtualNetwork maps a target-cluster router Pod event to the
// corresponding management-cluster VirtualNetwork. The request intentionally
// preserves the local-cluster identity because the VN status is stored on the
// management cluster, while the event originated on the target cluster.
func (r *VirtualNetworkReconciler) mapRouterPodToVirtualNetwork(_ context.Context, obj client.Object) []mcreconcile.Request {
	pod, ok := obj.(*corev1.Pod)
	if !ok {
		return nil
	}
	vnName := pod.Labels[osacRouterVirtualNetworkLabel]
	if vnName == "" {
		return nil
	}
	return []mcreconcile.Request{{
		Request:     reconcile.Request{NamespacedName: client.ObjectKey{Namespace: r.NetworkingNamespace, Name: vnName}},
		ClusterName: mc.ClusterName(""),
	}}
}

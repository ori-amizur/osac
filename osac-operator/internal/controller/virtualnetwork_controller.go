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
	"context"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/events"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/cluster"
	controllerutil "sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	mcbuilder "sigs.k8s.io/multicluster-runtime/pkg/builder"
	mchandler "sigs.k8s.io/multicluster-runtime/pkg/handler"
	mcmanager "sigs.k8s.io/multicluster-runtime/pkg/manager"
	mc "sigs.k8s.io/multicluster-runtime/pkg/multicluster"
	mcreconcile "sigs.k8s.io/multicluster-runtime/pkg/reconcile"

	"github.com/osac-project/osac/osac-operator/api/v1alpha1"
	"github.com/osac-project/osac/osac-operator/pkg/dispatcher"
	"github.com/osac-project/osac/osac-operator/pkg/provisioning"
)

const (
	osacVirtualNetworkFinalizer  = "osac.openshift.io/virtualnetwork-finalizer"
	virtualNetworkControllerName = "virtualnetwork-controller"
)

// VirtualNetworkReconciler reconciles a VirtualNetwork object
type VirtualNetworkReconciler struct {
	client.Client
	APIReader client.Reader
	Scheme    *runtime.Scheme
	Recorder  events.EventRecorder
	// mgr and targetCluster are stored for future multi-cluster target client resolution
	mgr                  mcmanager.Manager
	NetworkingNamespace  string
	ProvisioningProvider provisioning.ProvisioningProvider
	StatusPollInterval   time.Duration
	MaxJobHistory        int
	targetCluster        mc.ClusterName
	// Resolver resolves a NetworkClass to its registered managers. Nil when the
	// two-manager model isn't configured (no gRPC connection / networking namespace),
	// in which case the controller cannot resolve an implementation strategy for any
	// VirtualNetwork and requeues until Resolver is configured.
	Resolver *dispatcher.Resolver
	// NetworkProvisioningEnabled controls whether the controller dispatches AAP
	// provisioning jobs. When false, resources are set to Ready immediately
	// without triggering any infrastructure provisioning.
	NetworkProvisioningEnabled bool
}

// NewVirtualNetworkReconciler creates a new reconciler for VirtualNetwork resources.
func NewVirtualNetworkReconciler(
	mgr mcmanager.Manager,
	networkingNamespace string,
	provisioningProvider provisioning.ProvisioningProvider,
	statusPollInterval time.Duration,
	maxJobHistory int,
	targetCluster mc.ClusterName,
	resolver *dispatcher.Resolver,
) *VirtualNetworkReconciler {
	if mgr == nil {
		panic("mgr must not be nil")
	}
	if statusPollInterval <= 0 {
		statusPollInterval = provisioning.DefaultStatusPollInterval
	}
	if maxJobHistory <= 0 {
		maxJobHistory = provisioning.DefaultMaxJobHistory
	}
	return &VirtualNetworkReconciler{
		Client:               mgr.GetLocalManager().GetClient(),
		APIReader:            mgr.GetLocalManager().GetAPIReader(),
		Scheme:               mgr.GetLocalManager().GetScheme(),
		Recorder:             mgr.GetLocalManager().GetEventRecorder(virtualNetworkControllerName),
		mgr:                  mgr,
		NetworkingNamespace:  networkingNamespace,
		ProvisioningProvider: provisioningProvider,
		StatusPollInterval:   statusPollInterval,
		MaxJobHistory:        maxJobHistory,
		targetCluster:        targetCluster,
		Resolver:             resolver,
	}
}

// +kubebuilder:rbac:groups=osac.openshift.io,resources=virtualnetworks,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=osac.openshift.io,resources=virtualnetworks/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=osac.openshift.io,resources=virtualnetworks/finalizers,verbs=update
// +kubebuilder:rbac:groups=osac.openshift.io,resources=subnets,verbs=list
// +kubebuilder:rbac:groups=osac.openshift.io,resources=securitygroups,verbs=list
// +kubebuilder:rbac:groups=osac.openshift.io,resources=natgateways,verbs=list
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
func (r *VirtualNetworkReconciler) Reconcile(ctx context.Context, req mcreconcile.Request) (ctrl.Result, error) {
	log := ctrllog.FromContext(ctx)

	vnet := &v1alpha1.VirtualNetwork{}
	if err := r.Get(ctx, req.NamespacedName, vnet); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	val, exists := vnet.Annotations[osacManagementStateAnnotation]
	if vnet.ObjectMeta.DeletionTimestamp.IsZero() && exists && val == ManagementStateUnmanaged {
		log.Info("ignoring VirtualNetwork due to management-state annotation", "management-state", val)
		return ctrl.Result{}, nil
	}

	log.Info("start reconcile")

	oldstatus := vnet.Status.DeepCopy()

	var res ctrl.Result
	var err error
	if vnet.ObjectMeta.DeletionTimestamp.IsZero() {
		res, err = r.handleUpdate(ctx, vnet)
	} else {
		res, err = r.handleDelete(ctx, vnet)
	}

	if !equality.Semantic.DeepEqual(vnet.Status, *oldstatus) {
		log.Info("status requires update")
		if err := r.updateStatusWithRetry(ctx, client.ObjectKeyFromObject(vnet), vnet.Status); err != nil {
			return res, err
		}
	}

	log.Info("end reconcile")
	return res, err
}

// handleUpdate processes VirtualNetwork creation and updates
func (r *VirtualNetworkReconciler) handleUpdate(ctx context.Context, vnet *v1alpha1.VirtualNetwork) (ctrl.Result, error) {
	log := ctrllog.FromContext(ctx)

	// Add finalizer if not present
	if controllerutil.AddFinalizer(vnet, osacVirtualNetworkFinalizer) {
		if err := r.Update(ctx, vnet); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Set phase to Progressing only on first reconcile (empty phase).
	// Subsequent reconciles preserve the current phase — it gets updated
	// by OnSuccess/OnFailed callbacks in RunProvisioningLifecycle.
	if vnet.Status.Phase == "" {
		vnet.Status.Phase = v1alpha1.VirtualNetworkPhaseProgressing
	}

	if !r.NetworkProvisioningEnabled {
		vnet.Status.Phase = v1alpha1.VirtualNetworkPhaseReady
		setReadyConditionTrue(&vnet.Status.Conditions)
		return ctrl.Result{}, nil
	}

	// Resolve the dispatch plan once. The same plan determines the implementation
	// strategy, the fabric-manager presence snapshot, and the provisioning targets.
	plan, err := resolveDispatchPlan(ctx, r.Resolver, "VirtualNetwork", vnet.Spec.NetworkClass)
	if err != nil {
		return ctrl.Result{}, err
	}
	implementationStrategy := implementationStrategyFromDispatchPlan(plan)
	if implementationStrategy == "" {
		msg := fmt.Sprintf("NetworkClass '%s' has no fabric_manager or k8s_manager configured", vnet.Spec.NetworkClass)
		setReadyConditionBlocked(&vnet.Status.Conditions, v1alpha1.ReasonNoManagerConfigured, msg)
		log.Info("implementation strategy not set, requeueing", "virtualNetwork", vnet.Name)
		return ctrl.Result{RequeueAfter: defaultPreconditionRequeueInterval}, nil
	}

	k8sImplementationStrategy, transitCapability, annotationsChanged, err :=
		persistVirtualNetworkDispatchAnnotations(vnet, plan, implementationStrategy)
	if err != nil {
		return ctrl.Result{}, err
	}
	if annotationsChanged {
		log.Info("setting implementation-strategy/fabric-manager-configured/transit-capability annotations",
			"strategy", implementationStrategy,
			"k8sStrategy", k8sImplementationStrategy,
			"fabricManagerConfigured", plan.FabricTarget() != nil,
			"transitCapability", transitCapability)
		if transitCapability == transitCapabilityUnsupported && r.Recorder != nil {
			r.Recorder.Eventf(vnet, nil, corev1.EventTypeWarning, "UnsupportedTransitCapability", "Provisioning remains Phase-1-only: fabric manager %q has no supported transit contract", plan.FabricTarget().Manager.Name)
		}
		if err := r.Update(ctx, vnet); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	// Compute desired config version from spec and inherited implementation strategy
	desiredVersion, err := provisioning.ComputeDesiredConfigVersion(struct {
		Spec                      v1alpha1.VirtualNetworkSpec
		ImplementationStrategy    string
		K8sImplementationStrategy string
	}{vnet.Spec, implementationStrategy, k8sImplementationStrategy})
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to compute desired config version: %w", err)
	}
	vnet.Status.DesiredConfigVersion = desiredVersion

	// Set phase to Progressing only on first provision (empty phase) or when spec changed
	// after a previous success. Don't override Failed during backoff.
	if vnet.Status.Phase == "" || (vnet.Status.Phase == v1alpha1.VirtualNetworkPhaseReady &&
		!isVirtualNetworkConfigApplied(vnet.Status.ProvisioningJobs, vnet.Status.DesiredConfigVersion, plan)) {
		vnet.Status.Phase = v1alpha1.VirtualNetworkPhaseProgressing
	}

	// Handle provisioning
	result, err := r.handleProvisioning(ctx, vnet, plan)
	if err != nil {
		return result, err
	}

	// Router-pod recovery is independent of the normal VirtualNetwork
	// config-version lifecycle. A replacement Pod can occur without any VN or
	// Subnet spec change, so the target-cluster Pod watch reaches this path.
	if vnet.Spec.NetworkingType == v1alpha1.VirtualNetworkNetworkingTypeSecondary {
		baseReady := isVirtualNetworkConfigApplied(vnet.Status.ProvisioningJobs, vnet.Status.DesiredConfigVersion, plan)
		recoveryResult, recoveryErr := r.reconcileRouterPodRecovery(ctx, vnet, baseReady)
		if recoveryErr != nil {
			return result, recoveryErr
		}
		if recoveryResult.RequeueAfter > 0 &&
			(result.RequeueAfter == 0 || recoveryResult.RequeueAfter < result.RequeueAfter) {
			result.RequeueAfter = recoveryResult.RequeueAfter
		}
	}

	return result, nil
}

// handleProvisioning manages the provisioning job lifecycle for a VirtualNetwork.
// Dispatcher-backed VirtualNetworks with a fabric target use the shared
// multi-target lifecycle so the fabric and Kubernetes managers provision
// independently. Pure k8s-only VirtualNetworks and the legacy no-plan path keep
// the existing single-target lifecycle and job history. The variadic plan
// preserves the small unit-test helper contract used by older tests, which
// exercise the single-target path directly.
func (r *VirtualNetworkReconciler) handleProvisioning(
	ctx context.Context,
	vnet *v1alpha1.VirtualNetwork,
	plans ...*dispatcher.DispatchPlan,
) (ctrl.Result, error) {
	if r.ProvisioningProvider == nil {
		ctrllog.FromContext(ctx).Info("no provisioning provider configured, skipping provisioning")
		return ctrl.Result{}, nil
	}

	var plan *dispatcher.DispatchPlan
	if len(plans) > 0 {
		plan = plans[0]
	}

	fabricTarget := plan.FabricTarget()
	if fabricTarget == nil {
		return provisioning.RunProvisioningLifecycle(ctx, r.ProvisioningProvider, vnet,
			&provisioning.State{Jobs: &vnet.Status.ProvisioningJobs, DesiredConfigVersion: vnet.Status.DesiredConfigVersion},
			r.MaxJobHistory, r.StatusPollInterval,
			&provisioning.PollCallbacks{
				OnFailed: func(message string) {
					vnet.Status.Phase = v1alpha1.VirtualNetworkPhaseFailed
					setReadyConditionFailed(&vnet.Status.Conditions, message)
				},
				OnSuccess: func(_ provisioning.ProvisionStatus) {
					vnet.Status.Phase = v1alpha1.VirtualNetworkPhaseReady
					setReadyConditionTrue(&vnet.Status.Conditions)
				},
			},
			func() bool {
				return provisioning.CheckAPIServerForNonTerminalProvisionJob(ctx, r.APIReader, client.ObjectKeyFromObject(vnet), &v1alpha1.VirtualNetwork{}, func(obj client.Object) []v1alpha1.JobStatus {
					return obj.(*v1alpha1.VirtualNetwork).Status.ProvisioningJobs
				})
			},
			func() error {
				return r.updateStatusWithRetry(ctx, client.ObjectKeyFromObject(vnet), vnet.Status)
			},
		)
	}

	targetNames := []string{string(dispatcher.ManagerRoleFabric)}
	if plan.K8sTarget() != nil {
		targetNames = append(targetNames, string(dispatcher.ManagerRoleK8s))
	}

	onFailedFor := func(targetName string) func(string) {
		return func(message string) {
			vnet.Status.Phase = v1alpha1.VirtualNetworkPhaseFailed
			setReadyConditionFailed(&vnet.Status.Conditions, fmt.Sprintf("%s target: %s", targetName, message))
		}
	}
	onSuccess := func(_ provisioning.ProvisionStatus) {
		if allProvisionTargetsSucceeded(vnet.Status.ProvisioningJobs, vnet.Status.DesiredConfigVersion, targetNames...) {
			vnet.Status.Phase = v1alpha1.VirtualNetworkPhaseReady
			setReadyConditionTrue(&vnet.Status.Conditions)
		}
	}
	checkAPIServerFor := func(targetName string) func() bool {
		return func() bool {
			return provisioning.CheckAPIServerForNonTerminalProvisionJobAndTarget(
				ctx, r.APIReader, client.ObjectKeyFromObject(vnet), &v1alpha1.VirtualNetwork{}, func(obj client.Object) []v1alpha1.JobStatus {
					return obj.(*v1alpha1.VirtualNetwork).Status.ProvisioningJobs
				}, targetName)
		}
	}

	targets := []provisioning.JobTarget{
		{
			Name:           string(dispatcher.ManagerRoleFabric),
			Provider:       newDispatchTargetProvider(r.ProvisioningProvider, fabricTarget.Manager.Name),
			Callbacks:      &provisioning.PollCallbacks{OnFailed: onFailedFor(string(dispatcher.ManagerRoleFabric)), OnSuccess: onSuccess},
			CheckAPIServer: checkAPIServerFor(string(dispatcher.ManagerRoleFabric)),
			// Fabric was the sole VirtualNetwork target before dual dispatch was
			// introduced, so it owns any existing untargeted job history.
			AbsorbsLegacyHistory: true,
		},
	}
	if k8sTarget := plan.K8sTarget(); k8sTarget != nil {
		targets = append(targets, provisioning.JobTarget{
			Name:           string(dispatcher.ManagerRoleK8s),
			Provider:       newDispatchTargetProvider(r.ProvisioningProvider, k8sTarget.Manager.Name),
			Callbacks:      &provisioning.PollCallbacks{OnFailed: onFailedFor(string(dispatcher.ManagerRoleK8s)), OnSuccess: onSuccess},
			CheckAPIServer: checkAPIServerFor(string(dispatcher.ManagerRoleK8s)),
		})
	}

	return provisioning.RunMultiTargetProvisioningLifecycle(ctx, targets, vnet,
		&provisioning.State{Jobs: &vnet.Status.ProvisioningJobs, DesiredConfigVersion: vnet.Status.DesiredConfigVersion},
		r.MaxJobHistory, r.StatusPollInterval,
		func() error {
			return r.updateStatusWithRetry(ctx, client.ObjectKeyFromObject(vnet), vnet.Status)
		},
	)
}

// isVirtualNetworkConfigApplied reports whether the current desired version has
// succeeded for every dispatched target. Dispatcher-backed VirtualNetworks with
// a fabric target use target-tagged history for the fabric and, when present,
// Kubernetes jobs. The legacy no-plan and pure k8s-only paths use untargeted
// history.
func isVirtualNetworkConfigApplied(jobs []v1alpha1.JobStatus, desiredVersion string, plan *dispatcher.DispatchPlan) bool {
	if plan.FabricTarget() == nil {
		return provisioning.IsConfigApplied(&jobs, desiredVersion)
	}

	targetNames := []string{string(dispatcher.ManagerRoleFabric)}
	if plan.K8sTarget() != nil {
		targetNames = append(targetNames, string(dispatcher.ManagerRoleK8s))
	}
	return allProvisionTargetsSucceeded(jobs, desiredVersion, targetNames...)
}

// handleDelete processes VirtualNetwork deletion
func (r *VirtualNetworkReconciler) handleDelete(ctx context.Context, vnet *v1alpha1.VirtualNetwork) (ctrl.Result, error) {
	log := ctrllog.FromContext(ctx)
	log.Info("deleting virtual network")

	vnet.Status.Phase = v1alpha1.VirtualNetworkPhaseDeleting
	setTransitTeardownCondition(&vnet.Status.Conditions, transitTeardownStuckMessage(vnet.Status.ProvisioningJobs))

	// Base finalizer has already been removed, cleanup complete
	if !controllerutil.ContainsFinalizer(vnet, osacVirtualNetworkFinalizer) {
		return ctrl.Result{}, nil
	}

	// Gate: wait for all child resources referencing this VNet to be fully removed
	// before triggering the AAP deprovision job. Without this gate, the infrastructure
	// backend rejects the VNet deletion because children still exist, causing unnecessary
	// failed jobs and backoff delays.
	// Child resources reference the parent VN by its fulfillment-service UUID
	// (stored in the osac.openshift.io/virtualnetwork-uuid label), not by K8s name.
	vnetUUID := vnet.Labels[osacVirtualNetworkIDLabel]
	ns := vnet.Namespace

	subnetList := &v1alpha1.SubnetList{}
	if err := r.List(ctx, subnetList, client.InNamespace(ns)); err != nil {
		return ctrl.Result{}, fmt.Errorf("listing subnets: %w", err)
	}
	for i := range subnetList.Items {
		if subnetList.Items[i].Spec.VirtualNetwork == vnetUUID {
			log.Info("waiting for child Subnet to be deleted before deprovisioning VirtualNetwork",
				"subnet", subnetList.Items[i].Name)
			return ctrl.Result{RequeueAfter: defaultPreconditionRequeueInterval}, nil
		}
	}

	sgList := &v1alpha1.SecurityGroupList{}
	if err := r.List(ctx, sgList, client.InNamespace(ns)); err != nil {
		return ctrl.Result{}, fmt.Errorf("listing security groups: %w", err)
	}
	for i := range sgList.Items {
		if sgList.Items[i].Spec.VirtualNetwork == vnetUUID {
			log.Info("waiting for child SecurityGroup to be deleted before deprovisioning VirtualNetwork",
				"securityGroup", sgList.Items[i].Name)
			return ctrl.Result{RequeueAfter: defaultPreconditionRequeueInterval}, nil
		}
	}

	natgwList := &v1alpha1.NATGatewayList{}
	if err := r.List(ctx, natgwList, client.InNamespace(ns)); err != nil {
		return ctrl.Result{}, fmt.Errorf("listing NAT gateways: %w", err)
	}
	for i := range natgwList.Items {
		if natgwList.Items[i].Spec.VirtualNetwork == vnetUUID {
			log.Info("waiting for child NATGateway to be deleted before deprovisioning VirtualNetwork",
				"natGateway", natgwList.Items[i].Name)
			return ctrl.Result{RequeueAfter: defaultPreconditionRequeueInterval}, nil
		}
	}

	// Handle deprovisioning
	if vnet.Annotations[osacImplementationStrategyAnnotation] == "" {
		log.Info("skipping deprovisioning — resource was never provisioned")
	} else {
		result, err := r.handleDeprovisioning(ctx, vnet)
		if err != nil {
			return result, err
		}

		// If we need to requeue (jobs still running), do so
		if result.RequeueAfter > 0 {
			return result, nil
		}
	}

	// Deprovisioning complete or skipped, remove base finalizer
	if controllerutil.RemoveFinalizer(vnet, osacVirtualNetworkFinalizer) {
		if err := r.Update(ctx, vnet); err != nil {
			return ctrl.Result{}, err
		}
	}

	return ctrl.Result{}, nil
}

// transitTeardownStuckMessage extracts the explicit marker emitted by the
// fabric delete playbook when a VN-scoped transit resource cannot be removed.
// A later successful deprovision job clears the condition; a retry or an
// unrelated failure does not hide a still-existing cost-bearing resource.
func transitTeardownStuckMessage(jobs []v1alpha1.JobStatus) string {
	var stuck *v1alpha1.JobStatus
	var successful *v1alpha1.JobStatus
	for i := range jobs {
		job := &jobs[i]
		if job.Type != v1alpha1.JobTypeDeprovision {
			continue
		}
		if job.State == v1alpha1.JobStateSucceeded {
			if successful == nil || job.Timestamp.After(successful.Timestamp.Time) {
				successful = job
			}
		}
		if job.State == v1alpha1.JobStateFailed && strings.Contains(job.Message, transitTeardownStuckMarker) {
			if stuck == nil || job.Timestamp.After(stuck.Timestamp.Time) {
				stuck = job
			}
		}
	}
	if stuck == nil || (successful != nil && successful.Timestamp.After(stuck.Timestamp.Time)) {
		return ""
	}
	return stuck.Message
}

// handleDeprovisioning manages the deprovisioning job lifecycle for a VirtualNetwork.
// It triggers deprovisioning if needed and polls job status until completion.
func (r *VirtualNetworkReconciler) handleDeprovisioning(ctx context.Context, vnet *v1alpha1.VirtualNetwork) (ctrl.Result, error) {
	if r.ProvisioningProvider == nil {
		ctrllog.FromContext(ctx).Info("no provisioning provider configured, skipping deprovisioning")
		return ctrl.Result{}, nil
	}

	// handleUpdate always stamps osacImplementationStrategyAnnotation before any
	// provisioning job is ever triggered, so by the time a job exists the annotation
	// is guaranteed present. A VirtualNetwork deleted before its first successful
	// handleUpdate reconcile (e.g. immediately after creation, before the dispatcher
	// resolved a manager) has neither: nothing was ever provisioned, and — unlike
	// Subnet's playbooks — the VirtualNetwork delete playbook indexes the annotation
	// directly with no spec-field fallback, so triggering a deprovision job here would
	// just fail with an undefined implementation_strategy. Skip deprovisioning
	// entirely in that case; there is nothing to tear down.
	if vnet.Annotations[osacImplementationStrategyAnnotation] == "" && len(vnet.Status.ProvisioningJobs) == 0 {
		ctrllog.FromContext(ctx).Info("no implementation-strategy annotation and no job history, skipping deprovisioning")
		return ctrl.Result{}, nil
	}

	// Dispatcher-backed VirtualNetworks always tag their fabric jobs, even when
	// there is no k8s target. Only resources that predate dispatcher target tags
	// (and have no implementation-strategy annotation) use the legacy untargeted
	// lifecycle.
	if vnet.Annotations[osacImplementationStrategyAnnotation] == "" {
		result, done, err := provisioning.RunDeprovisioningLifecycle(ctx, r.ProvisioningProvider, vnet,
			&vnet.Status.ProvisioningJobs, r.MaxJobHistory, r.StatusPollInterval)
		if err != nil || !done {
			return result, err
		}
		return ctrl.Result{}, nil
	}

	targets := []provisioning.DeprovisionTarget{
		{
			Name:                 string(dispatcher.ManagerRoleFabric),
			Provider:             newDispatchTargetProvider(r.ProvisioningProvider, vnet.Annotations[osacImplementationStrategyAnnotation]),
			AbsorbsLegacyHistory: true,
		},
	}
	if k8sStrategy := vnet.Annotations[osacK8sImplementationStrategyAnnotation]; k8sStrategy != "" {
		targets = append(targets, provisioning.DeprovisionTarget{
			Name:     string(dispatcher.ManagerRoleK8s),
			Provider: newDispatchTargetProvider(r.ProvisioningProvider, k8sStrategy),
		})
	}

	result, done, err := provisioning.RunMultiTargetDeprovisioningLifecycle(ctx, targets, vnet,
		&vnet.Status.ProvisioningJobs, r.MaxJobHistory, r.StatusPollInterval)
	if err != nil || !done {
		return result, err
	}
	return ctrl.Result{}, nil
}

// updateStatusWithRetry updates the virtual network status with retry on conflict.
func (r *VirtualNetworkReconciler) updateStatusWithRetry(ctx context.Context, key client.ObjectKey, newStatus v1alpha1.VirtualNetworkStatus) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		latest := &v1alpha1.VirtualNetwork{}
		if err := r.Get(ctx, key, latest); err != nil {
			return err
		}
		latest.Status = newStatus
		return r.Status().Update(ctx, latest)
	})
}

// NetworkingNamespacePredicate filters events by namespace for networking resources.
func NetworkingNamespacePredicate(namespace string) predicate.Predicate {
	return predicate.NewPredicateFuncs(
		func(obj client.Object) bool {
			return obj.GetNamespace() == namespace
		},
	)
}

// SetupWithManager sets up the controller with the Manager.
func (r *VirtualNetworkReconciler) SetupWithManager(mgr mcmanager.Manager) error {
	return mcbuilder.ControllerManagedBy(mgr).
		For(&v1alpha1.VirtualNetwork{},
			mcbuilder.WithPredicates(NetworkingNamespacePredicate(r.NetworkingNamespace)),
			mcbuilder.WithEngageWithLocalCluster(true),
			mcbuilder.WithEngageWithProviderClusters(false)).
		Watches(
			&corev1.Pod{},
			func(_ mc.ClusterName, _ cluster.Cluster) handler.TypedEventHandler[client.Object, mcreconcile.Request] {
				return mchandler.TypedEnqueueRequestsFromMapFuncWithClusterPreservation(
					func(ctx context.Context, obj client.Object) []mcreconcile.Request {
						return r.mapRouterPodToVirtualNetwork(ctx, obj)
					},
				)
			},
			mcbuilder.WithPredicates(routerPodPredicate()),
			mcbuilder.WithEngageWithLocalCluster(true),
			mcbuilder.WithEngageWithProviderClusters(true),
			mcbuilder.WithClusterFilter(func(clusterName mc.ClusterName, _ cluster.Cluster) bool {
				return clusterName == r.targetCluster
			}),
		).
		Complete(r)
}

func routerPodPredicate() predicate.Predicate {
	return predicate.Funcs{
		CreateFunc: func(evt event.CreateEvent) bool {
			return isRouterPod(evt.Object)
		},
		UpdateFunc: func(evt event.UpdateEvent) bool {
			return isRouterPod(evt.ObjectNew) || isRouterPod(evt.ObjectOld)
		},
		DeleteFunc: func(evt event.DeleteEvent) bool {
			return isRouterPod(evt.Object)
		},
		GenericFunc: func(evt event.GenericEvent) bool {
			return isRouterPod(evt.Object)
		},
	}
}

func isRouterPod(obj client.Object) bool {
	return obj != nil && obj.GetLabels()[osacRouterPodLabel] == labelValueTrue &&
		obj.GetLabels()[osacRouterVirtualNetworkLabel] != ""
}

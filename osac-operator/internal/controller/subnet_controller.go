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
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	controllerutil "sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	mcbuilder "sigs.k8s.io/multicluster-runtime/pkg/builder"
	mchandler "sigs.k8s.io/multicluster-runtime/pkg/handler"
	mcmanager "sigs.k8s.io/multicluster-runtime/pkg/manager"
	mc "sigs.k8s.io/multicluster-runtime/pkg/multicluster"
	mcreconcile "sigs.k8s.io/multicluster-runtime/pkg/reconcile"

	bmfov1alpha1 "github.com/osac-project/osac/bare-metal-fulfillment-operator/api/v1alpha1"
	"github.com/osac-project/osac/osac-operator/api/v1alpha1"
	"github.com/osac-project/osac/osac-operator/helpers"
	privatev1 "github.com/osac-project/osac/osac-operator/internal/api/osac/private/v1"
	"github.com/osac-project/osac/osac-operator/pkg/dispatcher"
	"github.com/osac-project/osac/osac-operator/pkg/provisioning"
)

const (
	osacSubnetFinalizer = "osac.openshift.io/subnet-finalizer"
)

var ipAddressPoolGVK = schema.GroupVersionKind{
	Group:   "metallb.io",
	Version: "v1beta1",
	Kind:    "IPAddressPool",
}

// SubnetReconciler reconciles a Subnet object
type SubnetReconciler struct {
	client.Client
	APIReader client.Reader
	Scheme    *runtime.Scheme
	mgr       mcmanager.Manager
	// networkClassesClient fetches NetworkClass from the fulfillment-service
	// to read vip_prefix_length. Nil when gRPC is not configured.
	networkClassesClient privatev1.NetworkClassesClient
	NetworkingNamespace  string
	ProvisioningProvider provisioning.ProvisioningProvider
	// FabricRouteProvider reconciles the separate fabric-side route required for
	// Kubernetes-owned Secondary Subnets. It must never be used as the Subnet's
	// backend owner.
	FabricRouteProvider provisioning.ProvisioningProvider
	StatusPollInterval  time.Duration
	MaxJobHistory       int
	targetCluster       mc.ClusterName
	// Resolver resolves a NetworkClass to its registered managers. Nil when the
	// two-manager model isn't configured (no gRPC connection / networking namespace),
	// in which case the controller always uses the legacy implementation-strategy path.
	Resolver *dispatcher.Resolver
	// NetworkProvisioningEnabled controls whether the controller dispatches AAP
	// provisioning jobs. When false, resources are set to Ready immediately.
	NetworkProvisioningEnabled bool
}

// NewSubnetReconciler creates a new reconciler for Subnet resources.
func NewSubnetReconciler(
	mgr mcmanager.Manager,
	networkingNamespace string,
	provisioningProvider provisioning.ProvisioningProvider,
	statusPollInterval time.Duration,
	maxJobHistory int,
	targetCluster mc.ClusterName,
	resolver *dispatcher.Resolver,
	networkClassesClient privatev1.NetworkClassesClient,
) *SubnetReconciler {
	if mgr == nil {
		panic("mgr must not be nil")
	}
	if statusPollInterval <= 0 {
		statusPollInterval = provisioning.DefaultStatusPollInterval
	}
	if maxJobHistory <= 0 {
		maxJobHistory = provisioning.DefaultMaxJobHistory
	}
	return &SubnetReconciler{
		Client:               mgr.GetLocalManager().GetClient(),
		APIReader:            mgr.GetLocalManager().GetAPIReader(),
		Scheme:               mgr.GetLocalManager().GetScheme(),
		mgr:                  mgr,
		networkClassesClient: networkClassesClient,
		NetworkingNamespace:  networkingNamespace,
		ProvisioningProvider: provisioningProvider,
		StatusPollInterval:   statusPollInterval,
		MaxJobHistory:        maxJobHistory,
		targetCluster:        targetCluster,
		Resolver:             resolver,
	}
}

// +kubebuilder:rbac:groups=osac.openshift.io,resources=subnets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=osac.openshift.io,resources=subnets/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=osac.openshift.io,resources=subnets/finalizers,verbs=update
// +kubebuilder:rbac:groups=osac.openshift.io,resources=virtualnetworks,verbs=get;list;watch
// +kubebuilder:rbac:groups=osac.openshift.io,resources=computeinstances,verbs=list
// +kubebuilder:rbac:groups=osac.openshift.io,resources=baremetalinstances,verbs=list
// +kubebuilder:rbac:groups=metallb.io,resources=ipaddresspools,verbs=get;create;update;delete
// +kubebuilder:rbac:groups=coordination.k8s.io,resources=leases,verbs=get;create;update

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
func (r *SubnetReconciler) Reconcile(ctx context.Context, req mcreconcile.Request) (ctrl.Result, error) {
	log := ctrllog.FromContext(ctx)

	subnet := &v1alpha1.Subnet{}
	err := r.Client.Get(ctx, req.NamespacedName, subnet)
	if err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	val, exists := subnet.Annotations[osacManagementStateAnnotation]
	if subnet.ObjectMeta.DeletionTimestamp.IsZero() && exists && val == ManagementStateUnmanaged {
		log.Info("ignoring Subnet due to management-state annotation", "management-state", val)
		return ctrl.Result{}, nil
	}

	log.Info("start reconcile")

	oldstatus := subnet.Status.DeepCopy()

	var res ctrl.Result
	if subnet.ObjectMeta.DeletionTimestamp.IsZero() {
		res, err = r.handleUpdate(ctx, subnet)
	} else {
		res, err = r.handleDelete(ctx, subnet)
	}

	if !equality.Semantic.DeepEqual(subnet.Status, *oldstatus) {
		log.Info("status requires update")
		if err := r.updateStatusWithRetry(ctx, client.ObjectKeyFromObject(subnet), subnet.Status); err != nil {
			return res, err
		}
	}

	log.Info("end reconcile")
	return res, err
}

// updateStatusWithRetry updates the subnet status with retry on conflict.
func (r *SubnetReconciler) updateStatusWithRetry(ctx context.Context, key client.ObjectKey, newStatus v1alpha1.SubnetStatus) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		latest := &v1alpha1.Subnet{}
		if err := r.Get(ctx, key, latest); err != nil {
			return err
		}
		latest.Status = newStatus
		return r.Status().Update(ctx, latest)
	})
}

// SetupWithManager sets up the controller with the Manager.
func (r *SubnetReconciler) SetupWithManager(mgr mcmanager.Manager) error {
	return mcbuilder.ControllerManagedBy(mgr).
		For(&v1alpha1.Subnet{},
			mcbuilder.WithPredicates(NetworkingNamespacePredicate(r.NetworkingNamespace)),
			mcbuilder.WithEngageWithLocalCluster(true),
			mcbuilder.WithEngageWithProviderClusters(false)).
		Watches(
			&v1alpha1.VirtualNetwork{},
			mchandler.TypedLift(handler.EnqueueRequestsFromMapFunc(r.mapVirtualNetworkToSubnets)),
			mcbuilder.WithPredicates(NetworkingNamespacePredicate(r.NetworkingNamespace)),
			mcbuilder.WithEngageWithLocalCluster(true),
			mcbuilder.WithEngageWithProviderClusters(false),
		).
		Complete(r)
}

func (r *SubnetReconciler) mapVirtualNetworkToSubnets(ctx context.Context, obj client.Object) []reconcile.Request {
	vnet, ok := obj.(*v1alpha1.VirtualNetwork)
	if !ok {
		return nil
	}

	vnetUUID := vnet.Labels[osacVirtualNetworkIDLabel]
	if vnetUUID == "" {
		return nil
	}
	subnets := &v1alpha1.SubnetList{}
	if err := r.List(ctx, subnets, client.InNamespace(vnet.Namespace), client.MatchingLabels{
		osacVirtualNetworkIDLabel: vnetUUID,
	}); err != nil {
		return nil
	}

	requests := make([]reconcile.Request, 0, len(subnets.Items))
	for i := range subnets.Items {
		if subnets.Items[i].Spec.VirtualNetwork != vnetUUID {
			continue
		}
		requests = append(requests, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&subnets.Items[i])})
	}
	return requests
}

// getParentVirtualNetwork looks up the Subnet's parent VirtualNetwork by UUID label. A nil
// VirtualNetwork with a nil error means the caller should requeue (parent not found yet); a
// nil VirtualNetwork with a non-nil error means the caller should propagate the error.
func (r *SubnetReconciler) getParentVirtualNetwork(ctx context.Context, subnet *v1alpha1.Subnet) (*v1alpha1.VirtualNetwork, ctrl.Result, error) {
	vnetList := &v1alpha1.VirtualNetworkList{}
	err := r.List(ctx, vnetList,
		client.InNamespace(subnet.Namespace),
		client.MatchingLabels{osacVirtualNetworkIDLabel: subnet.Spec.VirtualNetwork},
	)
	if err != nil {
		return nil, ctrl.Result{}, err
	}
	if len(vnetList.Items) == 0 {
		ctrllog.FromContext(ctx).Info("parent VirtualNetwork not found, requeueing", "uuid", subnet.Spec.VirtualNetwork)
		return nil, ctrl.Result{RequeueAfter: defaultPreconditionRequeueInterval}, nil
	}
	if len(vnetList.Items) > 1 {
		return nil, ctrl.Result{}, fmt.Errorf(
			"expected exactly one parent VirtualNetwork with uuid %q but found %d",
			subnet.Spec.VirtualNetwork, len(vnetList.Items))
	}
	return &vnetList.Items[0], ctrl.Result{}, nil
}

//nolint:gocyclo // This reconciler intentionally sequences immutable dispatch, owner selection, and independent route/readiness gates.
func (r *SubnetReconciler) handleUpdate(ctx context.Context, subnet *v1alpha1.Subnet) (ctrl.Result, error) {
	log := ctrllog.FromContext(ctx)

	// Add finalizer if not present
	if controllerutil.AddFinalizer(subnet, osacSubnetFinalizer) {
		if err := r.Update(ctx, subnet); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Ensure V-Net lock lease exists for move_network_attachment serialization
	if err := r.ensureVNetLockLease(ctx, subnet); err != nil {
		return ctrl.Result{}, err
	}

	// Set phase to Progressing only on first reconcile (empty phase).
	// Subsequent reconciles preserve the current phase — it gets updated
	// by OnSuccess/OnFailed callbacks in RunProvisioningLifecycle.
	if subnet.Status.Phase == "" {
		subnet.Status.Phase = v1alpha1.SubnetPhaseProgressing
	}

	// When networking provisioning is disabled, skip AAP job dispatch and set Ready
	// immediately. IP address pool creation on the target cluster is also skipped
	// since there is no backend to configure in noop mode.
	if !r.NetworkProvisioningEnabled {
		subnet.Status.Phase = v1alpha1.SubnetPhaseReady
		setReadyConditionTrue(&subnet.Status.Conditions)
		return ctrl.Result{}, nil
	}

	// Get parent VirtualNetwork by UUID label to read implementation strategy
	vnet, result, err := r.getParentVirtualNetwork(ctx, subnet)
	if err != nil || vnet == nil {
		return result, err
	}

	// Resolve the dispatch plan: dispatcher path when the parent VirtualNetwork's
	// NetworkClass has a fabricManager registered (plan non-nil), else fall back to
	// whatever implementation-strategy annotation the parent VirtualNetwork's own
	// controller has already resolved and written onto it. Subnet is the only
	// resource kind whose plan can carry a k8s target alongside the fabric one — see
	// pkg/dispatcher's dispatch table.
	plan, err := resolveDispatchPlan(ctx, r.Resolver, "Subnet", vnet.Spec.NetworkClass)
	if err != nil {
		return ctrl.Result{}, err
	}

	networkingType := vnet.Spec.NetworkingType
	if networkingType == "" {
		// No CRD-level default exists for this field (defaulting happens upstream, in
		// fulfillment-service's Create validation) - a VirtualNetwork CR created
		// directly against the K8s API can still leave it empty.
		networkingType = v1alpha1.VirtualNetworkNetworkingTypePrimary
	}
	if networkingType != v1alpha1.VirtualNetworkNetworkingTypeSecondary && subnet.Spec.ImplementationStrategy != "" {
		return ctrl.Result{}, fmt.Errorf("subnet implementationStrategy is only valid for Secondary VirtualNetworks")
	}

	// Secondary Subnets have one explicit backend owner. The NetworkClass may
	// resolve both a fabric and a k8s manager because the parent Secondary
	// VirtualNetwork itself can require both kinds of resources, but those two
	// managers must never provision the same Subnet. An omitted strategy selects
	// the fabric target as the deployment default, preserving the current Netris
	// behavior.
	effectivePlan := plan
	ownerRole := ""
	implementationStrategy := vnet.Annotations[osacImplementationStrategyAnnotation]
	k8sStrategy := ""
	hasK8sTarget := false
	if networkingType == v1alpha1.VirtualNetworkNetworkingTypeSecondary {
		var ownerTarget *dispatcher.DispatchTarget
		var selectErr error
		effectivePlan, ownerTarget, selectErr = selectSecondarySubnetDispatchPlan(plan, subnet.Spec.ImplementationStrategy)
		if selectErr != nil {
			return ctrl.Result{}, selectErr
		}
		if ownerTarget != nil {
			implementationStrategy = ownerTarget.Manager.Name
			ownerRole = string(ownerTarget.Role)
		}
	} else {
		// Primary Subnets retain the existing dispatcher behavior. In particular,
		// a NetworkClass that intentionally uses dual dispatch for Primary resources
		// continues to produce both targets.
		// plan may be nil on the legacy path; the accessors are nil-safe.
		if fabricTarget := plan.FabricTarget(); fabricTarget != nil {
			implementationStrategy = fabricTarget.Manager.Name
		}
		if k8sTarget := plan.K8sTarget(); k8sTarget != nil {
			k8sStrategy = k8sTarget.Manager.Name
			hasK8sTarget = true
		}
	}
	if implementationStrategy == "" {
		log.Info("implementation strategy not set on parent VirtualNetwork, requeueing", "virtualNetwork", vnet.Name)
		return ctrl.Result{RequeueAfter: defaultPreconditionRequeueInterval}, nil
	}

	// Resolve VIP prefix length from NetworkClass (if gRPC is available)
	vipCIDR := ""
	if r.networkClassesClient != nil && vnet.Spec.NetworkClass != "" && subnet.Spec.IPv4CIDR != "" {
		var resolveErr error
		vipCIDR, resolveErr = r.resolveVIPCIDR(ctx, vnet.Spec.NetworkClass, subnet.Spec.IPv4CIDR)
		if resolveErr != nil {
			log.Error(resolveErr, "failed to resolve VIP CIDR from NetworkClass, requeueing",
				"networkClass", vnet.Spec.NetworkClass)
			return ctrl.Result{RequeueAfter: defaultPreconditionRequeueInterval}, nil
		}
	}

	if networkingType != v1alpha1.VirtualNetworkNetworkingTypeSecondary {
		// Reverse migration is only part of the historical Primary dual-dispatch
		// path. Secondary Subnets always have one selected owner.
		if requeue, deprovErr := r.deprovisionStaleK8sTarget(ctx, subnet, plan.K8sTarget()); deprovErr != nil {
			return ctrl.Result{}, deprovErr
		} else if requeue != nil {
			return *requeue, nil
		}
	}

	updated, err := r.updateSubnetStrategyAnnotations(ctx, subnet, implementationStrategy, k8sStrategy, vipCIDR,
		string(networkingType), vnet.Name, vnet.Spec.Region, hasK8sTarget, ownerRole)
	if err != nil {
		return ctrl.Result{}, err
	}
	if updated {
		return ctrl.Result{}, nil
	}

	// Compute desired config version from spec and inherited implementation strategy(ies).
	// K8sImplementationStrategy is included alongside the fabric one so that a
	// NetworkClass's k8sManager changing (with no other spec change) still bumps the
	// version and triggers re-provisioning of the k8s target.
	desiredVersion, err := provisioning.ComputeDesiredConfigVersion(struct {
		Spec                      v1alpha1.SubnetSpec
		ImplementationStrategy    string
		K8sImplementationStrategy string
		OwnerRole                 string
	}{subnet.Spec, implementationStrategy, k8sStrategy, ownerRole})
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to compute desired config version: %w", err)
	}
	subnet.Status.DesiredConfigVersion = desiredVersion

	// Set phase to Progressing only on first provision (empty phase) or when spec changed
	// after a previous success. Don't override Failed during backoff.
	if subnet.Status.Phase == "" ||
		(subnet.Status.Phase == v1alpha1.SubnetPhaseReady && !isSubnetConfigApplied(subnet.Status.ProvisioningJobs, subnet.Status.DesiredConfigVersion, effectivePlan, ownerRole)) {
		subnet.Status.Phase = v1alpha1.SubnetPhaseProgressing
	}

	// Handle provisioning
	result, err = r.handleProvisioning(ctx, subnet, effectivePlan)
	if err != nil {
		return result, err
	}

	routeResult, routeErr := r.handleFabricRouteProvisioning(ctx, subnet, vnet, ownerRole)
	if routeErr != nil {
		return routeResult, routeErr
	}
	if routeResult.RequeueAfter > 0 &&
		(result.RequeueAfter == 0 || routeResult.RequeueAfter < result.RequeueAfter) {
		result.RequeueAfter = routeResult.RequeueAfter
	}

	// A Secondary Subnet cannot be Ready while its parent router Pod is being
	// recovered. This is independent of the Subnet's own config-version job.
	readinessResult := r.reconcileParentRouterReadiness(subnet, vnet, effectivePlan)
	if readinessResult.RequeueAfter > 0 &&
		(result.RequeueAfter == 0 || readinessResult.RequeueAfter < result.RequeueAfter) {
		result.RequeueAfter = readinessResult.RequeueAfter
	}
	return result, nil
}

// ensureVNetLockLease creates a K8s Lease for V-Net mutex locking if it
// doesn't already exist. The Lease is owned by the Subnet CR and will be
// garbage collected when the Subnet is deleted.
func (r *SubnetReconciler) ensureVNetLockLease(ctx context.Context, subnet *v1alpha1.Subnet) error {
	lease := &coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{
			Name:      vnetLockLeaseName(subnet.Name),
			Namespace: subnet.Namespace,
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion:         v1alpha1.GroupVersion.String(),
					Kind:               "Subnet",
					Name:               subnet.Name,
					UID:                subnet.UID,
					Controller:         ptr.To(true),
					BlockOwnerDeletion: ptr.To(false),
				},
			},
		},
		Spec: coordinationv1.LeaseSpec{
			LeaseDurationSeconds: ptr.To(int32(120)),
		},
	}
	err := r.Create(ctx, lease)
	if apierrors.IsAlreadyExists(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("creating V-Net lock lease: %w", err)
	}
	ctrllog.FromContext(ctx).Info("created V-Net lock lease", "lease", lease.Name)
	return nil
}

// updateSubnetStrategyAnnotations stamps the implementation-strategy, k8s
// implementation-strategy, selected Subnet dispatch role, VIP CIDR, networking-type, parent-VirtualNetwork-name,
// and parent-VirtualNetwork-region
// annotations AAP playbooks rely on, persisting subnet if anything changed. The k8s
// annotation is compared and, when hasK8sTarget is false, removed unconditionally (not
// just when previously present) so a Subnet transitioning from dual-dispatch to
// fabric-only doesn't leave a stale k8s target for handleDeprovisioning to act on.
// networkingType and virtualNetworkName mirror the parent VirtualNetwork as resolved by
// the caller (getParentVirtualNetwork) — they're always present since every Subnet has a
// parent VN by the time this runs. Returns true when it persisted a change — the caller
// should return immediately in that case, since the change itself triggers a fresh
// reconcile that resumes with up-to-date annotations.
func (r *SubnetReconciler) updateSubnetStrategyAnnotations(ctx context.Context, subnet *v1alpha1.Subnet, implementationStrategy, k8sStrategy, vipCIDR, networkingType, virtualNetworkName, virtualNetworkRegion string, hasK8sTarget bool, ownerRole string) (bool, error) {
	if subnet.Annotations == nil {
		subnet.Annotations = make(map[string]string)
	}
	changed := false
	if subnet.Annotations[osacImplementationStrategyAnnotation] != implementationStrategy {
		subnet.Annotations[osacImplementationStrategyAnnotation] = implementationStrategy
		changed = true
	}
	if subnet.Annotations[osacK8sImplementationStrategyAnnotation] != k8sStrategy {
		if hasK8sTarget {
			subnet.Annotations[osacK8sImplementationStrategyAnnotation] = k8sStrategy
		} else {
			delete(subnet.Annotations, osacK8sImplementationStrategyAnnotation)
		}
		changed = true
	}
	if ownerRole != "" && subnet.Annotations[osacSubnetImplementationRoleAnnotation] != ownerRole {
		subnet.Annotations[osacSubnetImplementationRoleAnnotation] = ownerRole
		changed = true
	} else if ownerRole == "" && subnet.Annotations[osacSubnetImplementationRoleAnnotation] != "" {
		delete(subnet.Annotations, osacSubnetImplementationRoleAnnotation)
		changed = true
	}
	if vipCIDR != "" && subnet.Annotations[osacVIPCIDRAnnotation] != vipCIDR {
		subnet.Annotations[osacVIPCIDRAnnotation] = vipCIDR
		changed = true
	} else if vipCIDR == "" && subnet.Annotations[osacVIPCIDRAnnotation] != "" {
		delete(subnet.Annotations, osacVIPCIDRAnnotation)
		changed = true
	}
	if subnet.Annotations[osacNetworkingTypeAnnotation] != networkingType {
		subnet.Annotations[osacNetworkingTypeAnnotation] = networkingType
		changed = true
	}
	if subnet.Annotations[osacVirtualNetworkNameAnnotation] != virtualNetworkName {
		subnet.Annotations[osacVirtualNetworkNameAnnotation] = virtualNetworkName
		changed = true
	}
	if subnet.Annotations[osacVirtualNetworkRegionAnnotation] != virtualNetworkRegion {
		subnet.Annotations[osacVirtualNetworkRegionAnnotation] = virtualNetworkRegion
		changed = true
	}
	if !changed {
		return false, nil
	}
	ctrllog.FromContext(ctx).Info("updating annotations", "strategy", implementationStrategy, "k8sStrategy", k8sStrategy, "vipCIDR", vipCIDR)
	if err := r.Update(ctx, subnet); err != nil {
		return false, err
	}
	return true, nil
}

// deprovisionStaleK8sTarget handles the reverse-migration case: the NetworkClass
// dropped its k8sManager (currentK8sTarget is nil) while subnet still carries
// osacK8sImplementationStrategyAnnotation from a prior dual-dispatch reconcile. It
// deprovisions that now-stale k8s target and keeps both implementation-strategy
// annotations in place until deprovisioning completes — the mirror image of
// AbsorbsLegacyHistory's forward-migration story — so the k8s manager's resource
// isn't silently orphaned once the annotation (and with it, handleDeprovisioning's
// only record of the target) would otherwise be gone.
//
// Returns a non-nil result when the caller should return immediately (deprovisioning
// still in progress, or a status flush failed); a nil result and nil error mean the
// caller should proceed with its normal annotation bookkeeping.
func (r *SubnetReconciler) deprovisionStaleK8sTarget(ctx context.Context, subnet *v1alpha1.Subnet, currentK8sTarget *dispatcher.DispatchTarget) (*ctrl.Result, error) {
	existingK8sStrategy := subnet.Annotations[osacK8sImplementationStrategyAnnotation]
	if currentK8sTarget != nil || existingK8sStrategy == "" || r.ProvisioningProvider == nil {
		return nil, nil
	}

	_, done, err := provisioning.RunMultiTargetDeprovisioningLifecycle(ctx,
		[]provisioning.DeprovisionTarget{
			{Name: string(dispatcher.ManagerRoleK8s), Provider: newDispatchTargetProvider(r.ProvisioningProvider, existingK8sStrategy)},
		},
		subnet, &subnet.Status.ProvisioningJobs, r.MaxJobHistory, r.StatusPollInterval)
	if err != nil {
		return nil, fmt.Errorf("deprovisioning stale k8s target for subnet %s/%s: %w", subnet.Namespace, subnet.Name, err)
	}
	if done {
		return nil, nil
	}

	if err := r.updateStatusWithRetry(ctx, client.ObjectKeyFromObject(subnet), subnet.Status); err != nil {
		return nil, err
	}
	result := ctrl.Result{RequeueAfter: r.StatusPollInterval}
	return &result, nil
}

func (r *SubnetReconciler) handleDelete(ctx context.Context, subnet *v1alpha1.Subnet) (ctrl.Result, error) {
	log := ctrllog.FromContext(ctx)
	log.Info("deleting subnet")

	subnet.Status.Phase = v1alpha1.SubnetPhaseDeleting

	// Base finalizer has already been removed, cleanup complete
	if !controllerutil.ContainsFinalizer(subnet, osacSubnetFinalizer) {
		return ctrl.Result{}, nil
	}

	// Gate: wait for ComputeInstances with network attachments to this subnet to be
	// fully removed. Without this gate, the infrastructure backend rejects the subnet
	// deletion because instances still exist on it.
	subnetName := subnet.Name
	ns := subnet.Namespace

	ciList := &v1alpha1.ComputeInstanceList{}
	if err := r.List(ctx, ciList, client.InNamespace(ns)); err != nil {
		return ctrl.Result{}, fmt.Errorf("listing ComputeInstances: %w", err)
	}
	for i := range ciList.Items {
		for _, na := range ciList.Items[i].Spec.NetworkAttachments {
			if na.SubnetRef == subnetName {
				log.Info("waiting for ComputeInstance to be deleted before deprovisioning Subnet",
					"computeInstance", ciList.Items[i].Name)
				return ctrl.Result{RequeueAfter: defaultPreconditionRequeueInterval}, nil
			}
		}
	}

	// Gate: wait for BareMetalInstances with network attachments to this subnet.
	bmiList := &bmfov1alpha1.BareMetalInstanceList{}
	if err := r.List(ctx, bmiList, client.InNamespace(ns)); err != nil {
		return ctrl.Result{}, fmt.Errorf("listing BareMetalInstances: %w", err)
	}
	for i := range bmiList.Items {
		for _, na := range bmiList.Items[i].Spec.NetworkAttachments {
			if na.SubnetRef == subnetName {
				log.Info("waiting for BareMetalInstance to be deleted before deprovisioning Subnet",
					"bareMetalInstance", bmiList.Items[i].Name)
				return ctrl.Result{RequeueAfter: defaultPreconditionRequeueInterval}, nil
			}
		}
	}

	if subnet.Annotations[osacImplementationStrategyAnnotation] == "" {
		log.Info("skipping deprovisioning — resource was never provisioned")
	} else {
		// Remove the target-cluster IP address pool before AAP deprovisioning
		if err := r.deleteMetalLBIPAddressPool(ctx, subnet); err != nil {
			return ctrl.Result{}, fmt.Errorf("deleting MetalLB IPAddressPool: %w", err)
		}

		// Handle deprovisioning
		routeResult, routeErr := r.handleFabricRouteDeprovisioning(ctx, subnet)
		if routeErr != nil {
			return routeResult, routeErr
		}
		if routeResult.RequeueAfter > 0 {
			return routeResult, nil
		}

		result, err := r.handleDeprovisioning(ctx, subnet)
		if err != nil {
			return result, err
		}

		// If we need to requeue (jobs still running), do so
		if result.RequeueAfter > 0 {
			return result, nil
		}
	}

	// Deprovisioning complete or skipped, remove base finalizer
	if controllerutil.RemoveFinalizer(subnet, osacSubnetFinalizer) {
		if err := r.Update(ctx, subnet); err != nil {
			return ctrl.Result{}, err
		}
	}

	return ctrl.Result{}, nil
}

const fabricRouteTarget = string(dispatcher.ManagerRoleFabric)

// fabricRouteTransitCapabilities is the explicit registry of transit
// contracts whose fabric side can accept the backend-neutral route intent. A
// future agentless_net VLAN/LocalNet implementation adds its immutable
// capability here and its own AAP route role; the lifecycle below remains
// unchanged.
var fabricRouteTransitCapabilities = map[string]struct{}{
	transitCapabilityNetrisEVPN:            {},
	transitCapabilityAgentlessVLANLocalNet: {},
}

// subnetFabricRouteJobsExtractor extracts the independent route job history
// used by the route lifecycle's API-server duplicate-trigger check.
func subnetFabricRouteJobsExtractor(obj client.Object) []v1alpha1.JobStatus {
	return obj.(*v1alpha1.Subnet).Status.FabricRouteJobs //nolint:forcetypeassert // fixed by the lifecycle caller
}

func fabricTransitSupportsRoutes(vnet *v1alpha1.VirtualNetwork) bool {
	if vnet == nil {
		return false
	}
	_, supported := fabricRouteTransitCapabilities[vnet.Annotations[osacTransitCapabilityAnnotation]]
	return supported
}

// fabricRouteRequired reports whether this Subnet needs an auxiliary fabric
// route. The Subnet remains owned by Kubernetes; this is an independent route
// intent dispatched to the VN's selected fabric manager. Fabric-owned Subnets
// already have a native fabric representation and must not receive a duplicate
// explicit route.
func fabricRouteRequired(subnet *v1alpha1.Subnet, vnet *v1alpha1.VirtualNetwork, ownerRole string) bool {
	if vnet == nil || ownerRole != string(dispatcher.ManagerRoleK8s) {
		return false
	}
	if subnet.Annotations[osacNetworkingTypeAnnotation] != string(v1alpha1.VirtualNetworkNetworkingTypeSecondary) {
		return false
	}
	return fabricTransitSupportsRoutes(vnet) && subnet.Spec.IPv4CIDR != ""
}

func (r *SubnetReconciler) handleFabricRouteProvisioning(
	ctx context.Context, subnet *v1alpha1.Subnet, vnet *v1alpha1.VirtualNetwork, ownerRole string,
) (ctrl.Result, error) {
	if !fabricRouteRequired(subnet, vnet, ownerRole) {
		return ctrl.Result{}, nil
	}
	if r.FabricRouteProvider == nil {
		return ctrl.Result{}, fmt.Errorf("fabric route provider is not configured for Kubernetes-owned Secondary Subnet %s/%s", subnet.Namespace, subnet.Name)
	}
	if !allProvisionTargetsSucceeded(subnet.Status.ProvisioningJobs, subnet.Status.DesiredConfigVersion, ownerRole) {
		// The route must not be published before the selected Subnet owner has
		// completed its own operation. This also gives the AAP route task a
		// fully-created VN and transit contract to consume.
		return ctrl.Result{}, nil
	}

	fabricManager := vnet.Annotations[osacImplementationStrategyAnnotation]
	if fabricManager == "" {
		return ctrl.Result{}, fmt.Errorf("fabric route manager is not set on VirtualNetwork %s/%s", vnet.Namespace, vnet.Name)
	}
	if existing := subnet.Status.FabricRouteImplementationStrategy; existing != "" && existing != fabricManager {
		return ctrl.Result{}, fmt.Errorf("fabric route manager for Subnet %s/%s changed from %q to %q; route migration is not supported", subnet.Namespace, subnet.Name, existing, fabricManager)
	}
	subnet.Status.FabricRouteImplementationStrategy = fabricManager

	routeVersion, err := provisioning.ComputeDesiredConfigVersion(struct {
		VirtualNetwork string
		Subnet         string
		IPv4CIDR       string
		FabricManager  string
		TransitMode    string
	}{
		VirtualNetwork: subnet.Spec.VirtualNetwork,
		Subnet:         subnet.Name,
		IPv4CIDR:       subnet.Spec.IPv4CIDR,
		FabricManager:  fabricManager,
		TransitMode:    vnet.Annotations[osacTransitCapabilityAnnotation],
	})
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to compute fabric route config version: %w", err)
	}
	subnet.Status.FabricRouteConfigVersion = routeVersion

	setProgressing := func() {
		subnet.Status.Phase = v1alpha1.SubnetPhaseProgressing
		setReadyConditionBlocked(&subnet.Status.Conditions, v1alpha1.ReasonProgressing, "fabric route reconciliation is in progress")
	}
	setFailed := func(message string) {
		subnet.Status.Phase = v1alpha1.SubnetPhaseFailed
		setReadyConditionFailed(&subnet.Status.Conditions, "fabric route: "+message)
	}
	setSucceeded := func(_ provisioning.ProvisionStatus) {
		if allProvisionTargetsSucceeded(subnet.Status.ProvisioningJobs, subnet.Status.DesiredConfigVersion, ownerRole) &&
			allProvisionTargetsSucceeded(subnet.Status.FabricRouteJobs, subnet.Status.FabricRouteConfigVersion, fabricRouteTarget) {
			subnet.Status.Phase = v1alpha1.SubnetPhaseReady
			setReadyConditionTrue(&subnet.Status.Conditions)
		}
	}

	if provisioning.FindLatestJobByTypeAndTarget(subnet.Status.FabricRouteJobs, v1alpha1.JobTypeProvision, fabricRouteTarget) == nil {
		setProgressing()
	}

	result, err := provisioning.RunMultiTargetProvisioningLifecycle(ctx,
		[]provisioning.JobTarget{{
			Name:     fabricRouteTarget,
			Provider: newDispatchTargetProvider(r.FabricRouteProvider, fabricManager),
			Callbacks: &provisioning.PollCallbacks{
				OnFailed:  setFailed,
				OnSuccess: setSucceeded,
			},
			CheckAPIServer: func() bool {
				return provisioning.CheckAPIServerForNonTerminalProvisionJobAndTarget(
					ctx, r.APIReader, client.ObjectKeyFromObject(subnet), &v1alpha1.Subnet{}, subnetFabricRouteJobsExtractor, fabricRouteTarget)
			},
		}},
		subnet,
		&provisioning.State{Jobs: &subnet.Status.FabricRouteJobs, DesiredConfigVersion: subnet.Status.FabricRouteConfigVersion},
		r.MaxJobHistory,
		r.StatusPollInterval,
		func() error {
			return r.updateStatusWithRetry(ctx, client.ObjectKeyFromObject(subnet), subnet.Status)
		},
	)
	if err != nil {
		return result, fmt.Errorf("reconciling fabric route for Subnet %s/%s: %w", subnet.Namespace, subnet.Name, err)
	}
	return result, nil
}

// handleFabricRouteDeprovisioning withdraws the route before the Subnet owner
// is deprovisioned. It uses the persisted route manager and job history, so
// deletion remains deterministic even when the parent VirtualNetwork has
// already disappeared.
func (r *SubnetReconciler) handleFabricRouteDeprovisioning(ctx context.Context, subnet *v1alpha1.Subnet) (ctrl.Result, error) {
	if r.FabricRouteProvider == nil || len(subnet.Status.FabricRouteJobs) == 0 {
		return ctrl.Result{}, nil
	}

	fabricManager := subnet.Status.FabricRouteImplementationStrategy
	if fabricManager == "" {
		// Compatibility for status written before the explicit route-manager
		// field existed. New jobs always persist the manager in status.
		fabricManager = subnet.Annotations[osacImplementationStrategyAnnotation]
	}
	if fabricManager == "" {
		return ctrl.Result{}, fmt.Errorf("fabric route manager is missing for Subnet %s/%s", subnet.Namespace, subnet.Name)
	}

	result, done, err := provisioning.RunMultiTargetDeprovisioningLifecycle(ctx,
		[]provisioning.DeprovisionTarget{{
			Name:     fabricRouteTarget,
			Provider: newDispatchTargetProvider(r.FabricRouteProvider, fabricManager),
		}},
		subnet,
		&subnet.Status.FabricRouteJobs,
		r.MaxJobHistory,
		r.StatusPollInterval,
	)
	if err != nil {
		return result, fmt.Errorf("withdrawing fabric route for Subnet %s/%s: %w", subnet.Namespace, subnet.Name, err)
	}
	if !done {
		if err := r.updateStatusWithRetry(ctx, client.ObjectKeyFromObject(subnet), subnet.Status); err != nil {
			return ctrl.Result{}, err
		}
	}
	return result, nil
}

// subnetProvisioningJobsExtractor extracts the Subnet-typed jobs array used by
// CheckAPIServerForNonTerminalProvisionJob(AndTarget) to read jobs from a fresh
// API server copy of the resource.
func subnetProvisioningJobsExtractor(obj client.Object) []v1alpha1.JobStatus {
	return obj.(*v1alpha1.Subnet).Status.ProvisioningJobs //nolint:forcetypeassert // always called with a *v1alpha1.Subnet
}

// handleProvisioning manages the provisioning job lifecycle for a Subnet. When plan
// has no fabric target (the no-dispatcher legacy path), this is the single-target
// RunProvisioningLifecycle unchanged, using fully untargeted ("") job history. When
// plan has a fabric target — with or without an accompanying k8s target — it drives
// each resolved target independently via RunMultiTargetProvisioningLifecycle, always
// tagging the fabric target's jobs "fabric" (never leaving it untargeted). Keeping the
// fabric target consistently tagged, whether or not a k8s target is currently present,
// means transitioning into or out of dual-dispatch (a NetworkClass gaining or dropping
// a k8sManager) reuses the fabric target's existing job history and config version
// instead of re-triggering a duplicate job — see AbsorbsLegacyHistory below for the one
// exception (the initial migration off pre-dispatcher untargeted history). The Subnet
// only reaches Ready once allProvisionTargetsSucceeded reports every resolved target's
// latest job succeeded at the current desired config version — one target succeeding
// does not flip Ready on its own, and one target failing/backing off does not block
// another target's independent retry.
func (r *SubnetReconciler) handleProvisioning(ctx context.Context, subnet *v1alpha1.Subnet, plan *dispatcher.DispatchPlan) (ctrl.Result, error) {
	if r.ProvisioningProvider == nil {
		ctrllog.FromContext(ctx).Info("no provisioning provider configured, skipping provisioning")
		return ctrl.Result{}, nil
	}

	var result ctrl.Result
	var err error
	if subnet.Annotations[osacSubnetImplementationRoleAnnotation] != "" {
		result, err = r.handleSingleOwnerProvisioning(ctx, subnet, plan)
	} else if plan.FabricTarget() == nil {
		// No-dispatcher legacy path: implementationStrategy came from the parent
		// VirtualNetwork spec's annotation rather than a resolved DispatchPlan. Job
		// history for these Subnets has always been untargeted, so keep using the
		// fully single-target lifecycle unchanged.
		result, err = provisioning.RunProvisioningLifecycle(ctx, r.ProvisioningProvider, subnet,
			&provisioning.State{Jobs: &subnet.Status.ProvisioningJobs, DesiredConfigVersion: subnet.Status.DesiredConfigVersion},
			r.MaxJobHistory, r.StatusPollInterval,
			&provisioning.PollCallbacks{
				OnFailed: func(message string) {
					subnet.Status.Phase = v1alpha1.SubnetPhaseFailed
					setReadyConditionFailed(&subnet.Status.Conditions, message)
				},
				OnSuccess: func(_ provisioning.ProvisionStatus) {
					subnet.Status.Phase = v1alpha1.SubnetPhaseReady
					setReadyConditionTrue(&subnet.Status.Conditions)
				},
			},
			func() bool {
				return provisioning.CheckAPIServerForNonTerminalProvisionJob(ctx, r.APIReader, client.ObjectKeyFromObject(subnet), &v1alpha1.Subnet{}, subnetProvisioningJobsExtractor)
			},
			func() error {
				return r.updateStatusWithRetry(ctx, client.ObjectKeyFromObject(subnet), subnet.Status)
			},
		)
	} else {
		fabricTarget := plan.FabricTarget()
		k8sTarget := plan.K8sTarget()
		fabricName := string(dispatcher.ManagerRoleFabric)
		k8sName := string(dispatcher.ManagerRoleK8s)

		targetNames := []string{fabricName}
		if k8sTarget != nil {
			targetNames = append(targetNames, k8sName)
		}

		onFailedFor := func(targetName string) func(string) {
			return func(message string) {
				subnet.Status.Phase = v1alpha1.SubnetPhaseFailed
				setReadyConditionFailed(&subnet.Status.Conditions, fmt.Sprintf("%s target: %s", targetName, message))
			}
		}
		onSuccess := func(_ provisioning.ProvisionStatus) {
			if allProvisionTargetsSucceeded(subnet.Status.ProvisioningJobs, subnet.Status.DesiredConfigVersion, targetNames...) {
				subnet.Status.Phase = v1alpha1.SubnetPhaseReady
				setReadyConditionTrue(&subnet.Status.Conditions)
			}
		}
		checkAPIServerFor := func(targetName string) func() bool {
			return func() bool {
				return provisioning.CheckAPIServerForNonTerminalProvisionJobAndTarget(
					ctx, r.APIReader, client.ObjectKeyFromObject(subnet), &v1alpha1.Subnet{}, subnetProvisioningJobsExtractor, targetName)
			}
		}

		targets := []provisioning.JobTarget{
			{
				Name:           fabricName,
				Provider:       newDispatchTargetProvider(r.ProvisioningProvider, fabricTarget.Manager.Name),
				Callbacks:      &provisioning.PollCallbacks{OnFailed: onFailedFor(fabricName), OnSuccess: onSuccess},
				CheckAPIServer: checkAPIServerFor(fabricName),
				// Subnet was fabric-only (single, untargeted job history) before the
				// dispatcher path existed, so fabric inherits any pre-existing
				// Target=="" jobs the first time this NetworkClass resolves a plan.
				AbsorbsLegacyHistory: true,
			},
		}
		if k8sTarget != nil {
			targets = append(targets, provisioning.JobTarget{
				Name:           k8sName,
				Provider:       newDispatchTargetProvider(r.ProvisioningProvider, k8sTarget.Manager.Name),
				Callbacks:      &provisioning.PollCallbacks{OnFailed: onFailedFor(k8sName), OnSuccess: onSuccess},
				CheckAPIServer: checkAPIServerFor(k8sName),
			})
		}

		result, err = provisioning.RunMultiTargetProvisioningLifecycle(ctx, targets, subnet,
			&provisioning.State{Jobs: &subnet.Status.ProvisioningJobs, DesiredConfigVersion: subnet.Status.DesiredConfigVersion},
			r.MaxJobHistory, r.StatusPollInterval,
			func() error {
				return r.updateStatusWithRetry(ctx, client.ObjectKeyFromObject(subnet), subnet.Status)
			},
		)
	}
	if err != nil {
		return result, err
	}

	// Create MetalLB IPAddressPool after provisioning succeeds, outside the
	// callback so errors are returned to the reconcile loop for retry.
	if subnet.Status.Phase == v1alpha1.SubnetPhaseReady {
		if poolErr := r.ensureMetalLBIPAddressPool(ctx, subnet); poolErr != nil {
			return ctrl.Result{}, fmt.Errorf("creating MetalLB IPAddressPool: %w", poolErr)
		}
	}

	return result, nil
}

// handleSingleOwnerProvisioning manages a Secondary Subnet whose owner was
// selected from SubnetSpec. The dispatch plan has already been reduced to one
// target, and the job is tagged with that target role so status and deletion
// continue to identify the same owner even after the parent VirtualNetwork is
// gone.
func (r *SubnetReconciler) handleSingleOwnerProvisioning(
	ctx context.Context,
	subnet *v1alpha1.Subnet,
	plan *dispatcher.DispatchPlan,
) (ctrl.Result, error) {
	if plan == nil || len(plan.Targets) != 1 {
		return ctrl.Result{}, fmt.Errorf("Secondary Subnet owner selection must resolve exactly one dispatch target") //nolint:staticcheck // preserve the existing API error text
	}
	target := plan.Targets[0]
	targetName := string(target.Role)
	if targetName == "" {
		return ctrl.Result{}, fmt.Errorf("Secondary Subnet owner selection resolved an empty dispatch role") //nolint:staticcheck // preserve the existing API error text
	}

	onFailed := func(message string) {
		subnet.Status.Phase = v1alpha1.SubnetPhaseFailed
		setReadyConditionFailed(&subnet.Status.Conditions, fmt.Sprintf("%s target: %s", targetName, message))
	}
	onSuccess := func(_ provisioning.ProvisionStatus) {
		if allProvisionTargetsSucceeded(subnet.Status.ProvisioningJobs, subnet.Status.DesiredConfigVersion, targetName) {
			subnet.Status.Phase = v1alpha1.SubnetPhaseReady
			setReadyConditionTrue(&subnet.Status.Conditions)
		}
	}

	result, err := provisioning.RunMultiTargetProvisioningLifecycle(ctx,
		[]provisioning.JobTarget{{
			Name:      targetName,
			Provider:  newDispatchTargetProvider(r.ProvisioningProvider, target.Manager.Name),
			Callbacks: &provisioning.PollCallbacks{OnFailed: onFailed, OnSuccess: onSuccess},
			CheckAPIServer: func() bool {
				return provisioning.CheckAPIServerForNonTerminalProvisionJobAndTarget(
					ctx, r.APIReader, client.ObjectKeyFromObject(subnet), &v1alpha1.Subnet{}, subnetProvisioningJobsExtractor, targetName)
			},
			// A fabric-owned Secondary Subnet can inherit pre-dispatcher history.
			AbsorbsLegacyHistory: target.Role == dispatcher.ManagerRoleFabric,
		}},
		subnet,
		&provisioning.State{Jobs: &subnet.Status.ProvisioningJobs, DesiredConfigVersion: subnet.Status.DesiredConfigVersion},
		r.MaxJobHistory,
		r.StatusPollInterval,
		func() error {
			return r.updateStatusWithRetry(ctx, client.ObjectKeyFromObject(subnet), subnet.Status)
		},
	)
	if err != nil {
		return result, err
	}

	return result, nil
}

// isSubnetConfigApplied reports whether the current desired config version has been
// successfully applied to every target the plan resolves. On the dispatcher path,
// provision jobs are tagged by target ("fabric"/"k8s") rather than left untagged, so
// this must delegate to allProvisionTargetsSucceeded — the untargeted
// provisioning.IsConfigApplied would never find a match against tagged job history
// and would regress Ready back to Progressing on every reconcile. Falls back to
// provisioning.IsConfigApplied for the no-dispatcher legacy path (plan has no fabric
// target), whose job history has always been untargeted.
func isSubnetConfigApplied(jobs []v1alpha1.JobStatus, desiredVersion string, plan *dispatcher.DispatchPlan, ownerRole string) bool {
	if ownerRole != "" {
		return allProvisionTargetsSucceeded(jobs, desiredVersion, ownerRole)
	}
	fabricTarget := plan.FabricTarget()
	if fabricTarget == nil {
		return provisioning.IsConfigApplied(&jobs, desiredVersion)
	}
	targetNames := []string{string(dispatcher.ManagerRoleFabric)}
	if plan.K8sTarget() != nil {
		targetNames = append(targetNames, string(dispatcher.ManagerRoleK8s))
	}
	return allProvisionTargetsSucceeded(jobs, desiredVersion, targetNames...)
}

// allProvisionTargetsSucceeded reports whether every named target's most recent
// provision job succeeded at desiredVersion (or is a pre-Target legacy success with
// ConfigVersion == "" — mirrors IsConfigApplied's same accommodation). Used from
// each target's OnSuccess callback to decide whether the aggregate Subnet Phase can
// flip to Ready: Ready requires ALL targets independently confirmed successful, not
// just the one whose callback just fired.
func allProvisionTargetsSucceeded(jobs []v1alpha1.JobStatus, desiredVersion string, targetNames ...string) bool {
	if len(targetNames) == 0 {
		return false
	}
	for _, name := range targetNames {
		job := provisioning.FindLatestJobByTypeAndTarget(jobs, v1alpha1.JobTypeProvision, name)
		if job == nil || job.State != v1alpha1.JobStateSucceeded {
			return false
		}
		if job.ConfigVersion != desiredVersion && job.ConfigVersion != "" {
			return false
		}
	}
	return true
}

// handleDeprovisioning manages the deprovisioning job lifecycle for a Subnet. It
// trusts the annotations persisted by handleUpdate rather than re-resolving the
// DispatchPlan against the parent VirtualNetwork's NetworkClass, since the parent
// may already be gone or deleting concurrently by the time a Subnet is deleted.
// Branches on osacImplementationStrategyAnnotation (fabric), not the k8s one: once
// handleProvisioning has run at least once, fabric's job history is always tagged
// "fabric" (never left untargeted, even fabric-only — see its doc comment), so
// deprovisioning must go through the same "fabric"-tagged multi-target path to find
// it, regardless of whether a k8s target is currently also present. Only a Subnet
// that predates this feature and was deleted before ever being updated again — i.e.
// osacImplementationStrategyAnnotation was never stamped — falls back to the
// original single-target, fully untargeted RunDeprovisioningLifecycle. The fabric
// target's AbsorbsLegacyHistory absorbs any such untargeted history that does exist,
// so it's found rather than orphaned into a separate, disconnected Target=="" job.
// When the k8s annotation is also present, both managers are torn down in parallel,
// and the finalizer is only removed once both reach a terminal, non-blocking state.
func (r *SubnetReconciler) handleDeprovisioning(ctx context.Context, subnet *v1alpha1.Subnet) (ctrl.Result, error) {
	if r.ProvisioningProvider == nil {
		ctrllog.FromContext(ctx).Info("no provisioning provider configured, skipping deprovisioning")
		return ctrl.Result{}, nil
	}

	// Secondary Subnets persist their single selected dispatch role because the
	// parent VirtualNetwork may already be gone. Deprovision exactly that target;
	// never reconstruct a dual-dispatch operation during deletion.
	if ownerRole := subnet.Annotations[osacSubnetImplementationRoleAnnotation]; ownerRole != "" {
		strategy := subnet.Annotations[osacImplementationStrategyAnnotation]
		result, done, err := provisioning.RunMultiTargetDeprovisioningLifecycle(ctx,
			[]provisioning.DeprovisionTarget{{
				Name:                 ownerRole,
				Provider:             newDispatchTargetProvider(r.ProvisioningProvider, strategy),
				AbsorbsLegacyHistory: ownerRole == string(dispatcher.ManagerRoleFabric),
			}},
			subnet,
			&subnet.Status.ProvisioningJobs,
			r.MaxJobHistory,
			r.StatusPollInterval,
		)
		if err != nil || !done {
			return result, err
		}
		return ctrl.Result{}, nil
	}

	fabricStrategy := subnet.Annotations[osacImplementationStrategyAnnotation]
	if fabricStrategy == "" {
		result, done, err := provisioning.RunDeprovisioningLifecycle(ctx, r.ProvisioningProvider, subnet,
			&subnet.Status.ProvisioningJobs, r.MaxJobHistory, r.StatusPollInterval)
		if err != nil || !done {
			return result, err
		}
		return ctrl.Result{}, nil
	}

	targets := []provisioning.DeprovisionTarget{
		// AbsorbsLegacyHistory: true — see the matching comment in handleProvisioning.
		{Name: string(dispatcher.ManagerRoleFabric), Provider: newDispatchTargetProvider(r.ProvisioningProvider, fabricStrategy), AbsorbsLegacyHistory: true},
	}
	if k8sStrategy := subnet.Annotations[osacK8sImplementationStrategyAnnotation]; k8sStrategy != "" {
		targets = append(targets, provisioning.DeprovisionTarget{Name: string(dispatcher.ManagerRoleK8s), Provider: newDispatchTargetProvider(r.ProvisioningProvider, k8sStrategy)})
	}

	result, done, err := provisioning.RunMultiTargetDeprovisioningLifecycle(ctx, targets, subnet,
		&subnet.Status.ProvisioningJobs, r.MaxJobHistory, r.StatusPollInterval)
	if err != nil || !done {
		return result, err
	}
	return ctrl.Result{}, nil
}

// resolveVIPCIDR fetches the NetworkClass from the fulfillment-service and
// computes the VIP sub-range CIDR from the subnet's IPv4 CIDR and the
// NetworkClass's vip_prefix_length. Returns empty string if vip_prefix_length
// is not set.
func (r *SubnetReconciler) resolveVIPCIDR(ctx context.Context, networkClassID, subnetIPv4CIDR string) (string, error) {
	resp, err := r.networkClassesClient.Get(ctx, &privatev1.NetworkClassesGetRequest{Id: networkClassID})
	if err != nil {
		return "", fmt.Errorf("fetching NetworkClass %q: %w", networkClassID, err)
	}
	nc := resp.GetObject()
	if nc == nil || nc.GetSpec() == nil || !nc.GetSpec().HasVipPrefixLength() {
		return "", nil
	}
	vipPrefixLength := int(nc.GetSpec().GetVipPrefixLength())
	return helpers.FormatVIPRangeCIDR(subnetIPv4CIDR, vipPrefixLength)
}

func ipAddressPoolName(subnetName string) string {
	return "osac-subnet-" + subnetName
}

// ensureMetalLBIPAddressPool creates or updates the MetalLB IPAddressPool on
// the target cluster. Skipped when no VIP CIDR annotation is set or when the
// multi-cluster manager is not configured.
func (r *SubnetReconciler) ensureMetalLBIPAddressPool(ctx context.Context, subnet *v1alpha1.Subnet) error {
	vipCIDR := subnet.Annotations[osacVIPCIDRAnnotation]
	if vipCIDR == "" || r.mgr == nil {
		return nil
	}

	targetClient, err := getTargetClient(ctx, r.mgr, r.targetCluster)
	if err != nil {
		return fmt.Errorf("getting target cluster client: %w", err)
	}

	pool := &unstructured.Unstructured{}
	pool.SetGroupVersionKind(ipAddressPoolGVK)
	pool.SetName(ipAddressPoolName(subnet.Name))
	pool.SetNamespace(externalIPDefaultMetalLBNamespace)
	pool.SetLabels(map[string]string{
		osacPrefix + "/subnet": subnet.Name,
	})

	if err := unstructured.SetNestedField(pool.Object, false, "spec", "autoAssign"); err != nil {
		return fmt.Errorf("setting autoAssign: %w", err)
	}
	if err := unstructured.SetNestedField(pool.Object, true, "spec", "avoidBuggyIPs"); err != nil {
		return fmt.Errorf("setting avoidBuggyIPs: %w", err)
	}
	if err := unstructured.SetNestedSlice(pool.Object, []interface{}{vipCIDR}, "spec", "addresses"); err != nil {
		return fmt.Errorf("setting addresses: %w", err)
	}

	existing := &unstructured.Unstructured{}
	existing.SetGroupVersionKind(ipAddressPoolGVK)
	err = targetClient.Get(ctx, types.NamespacedName{Namespace: externalIPDefaultMetalLBNamespace, Name: pool.GetName()}, existing)
	if apierrors.IsNotFound(err) {
		ctrllog.FromContext(ctx).Info("creating MetalLB IPAddressPool", "name", pool.GetName(), "addresses", vipCIDR)
		return targetClient.Create(ctx, pool)
	}
	if err != nil {
		return fmt.Errorf("checking existing IPAddressPool: %w", err)
	}

	// Update if addresses changed
	existingAddrs, found, nestedErr := unstructured.NestedStringSlice(existing.Object, "spec", "addresses")
	if nestedErr != nil {
		return fmt.Errorf("reading existing IPAddressPool addresses: %w", nestedErr)
	}
	if !found || len(existingAddrs) != 1 || existingAddrs[0] != vipCIDR {
		existing.Object["spec"] = pool.Object["spec"]
		existing.SetLabels(pool.GetLabels())
		ctrllog.FromContext(ctx).Info("updating MetalLB IPAddressPool", "name", pool.GetName(), "addresses", vipCIDR)
		return targetClient.Update(ctx, existing)
	}

	return nil
}

// deleteMetalLBIPAddressPool removes the MetalLB IPAddressPool from the target
// cluster. NotFound errors are ignored. Skipped when the multi-cluster manager
// is not configured.
func (r *SubnetReconciler) deleteMetalLBIPAddressPool(ctx context.Context, subnet *v1alpha1.Subnet) error {
	if r.mgr == nil || subnet.Annotations[osacVIPCIDRAnnotation] == "" {
		return nil
	}

	targetClient, err := getTargetClient(ctx, r.mgr, r.targetCluster)
	if err != nil {
		return fmt.Errorf("getting target cluster client: %w", err)
	}

	pool := &unstructured.Unstructured{}
	pool.SetGroupVersionKind(ipAddressPoolGVK)
	pool.SetName(ipAddressPoolName(subnet.Name))
	pool.SetNamespace(externalIPDefaultMetalLBNamespace)

	err = targetClient.Delete(ctx, pool)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("deleting IPAddressPool %q: %w", pool.GetName(), err)
	}
	ctrllog.FromContext(ctx).Info("deleted MetalLB IPAddressPool", "name", pool.GetName())
	return nil
}

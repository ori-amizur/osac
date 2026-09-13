package controller

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1alpha1 "github.com/osac-project/osac/osac-operator/api/v1alpha1"
	"github.com/osac-project/osac/osac-operator/pkg/provisioning"
)

var _ = Describe("VirtualNetwork router Pod recovery", func() {
	It("starts exactly one recovery job for each replacement Pod UID", func() {
		ctx := context.Background()
		provider := &routerRecoveryTestProvider{
			statuses: []provisioning.ProvisionStatus{{
				State:   v1alpha1.JobStateSucceeded,
				Message: "repaired",
			}},
		}
		vnet := recoveryTestVirtualNetwork()
		pod := recoveryTestPod("pod-one")
		client := recoveryTestClient(vnet, pod)
		reconciler := &VirtualNetworkReconciler{
			Client:               client,
			ProvisioningProvider: provider,
			StatusPollInterval:   time.Second,
		}

		_, err := reconciler.reconcileRouterPodRecovery(ctx, vnet, true)
		Expect(err).NotTo(HaveOccurred())
		Expect(provider.triggerCount).To(Equal(1))
		Expect(vnet.Status.RouterPodRecovery.PodUID).To(Equal("pod-one"))

		_, err = reconciler.reconcileRouterPodRecovery(ctx, vnet, true)
		Expect(err).NotTo(HaveOccurred())
		Expect(provider.triggerCount).To(Equal(1))
		Expect(vnet.Status.RouterPodRecovery.State).To(Equal(v1alpha1.JobStateSucceeded))
		Expect(vnet.Status.Phase).To(Equal(v1alpha1.VirtualNetworkPhaseReady))

		Expect(client.Delete(ctx, pod)).To(Succeed())
		replacement := recoveryTestPod("pod-two")
		Expect(client.Create(ctx, replacement)).To(Succeed())

		_, err = reconciler.reconcileRouterPodRecovery(ctx, vnet, true)
		Expect(err).NotTo(HaveOccurred())
		Expect(provider.triggerCount).To(Equal(2))
		Expect(vnet.Status.RouterPodRecovery.PodUID).To(Equal("pod-two"))
	})

	It("keeps a failed job from being duplicated until its retry time", func() {
		ctx := context.Background()
		provider := &routerRecoveryTestProvider{
			statuses: []provisioning.ProvisionStatus{{
				State:   v1alpha1.JobStateFailed,
				Message: "repair failed",
			}},
		}
		vnet := recoveryTestVirtualNetwork()
		client := recoveryTestClient(vnet, recoveryTestPod("pod-one"))
		reconciler := &VirtualNetworkReconciler{
			Client:               client,
			ProvisioningProvider: provider,
			StatusPollInterval:   time.Second,
		}

		_, err := reconciler.reconcileRouterPodRecovery(ctx, vnet, true)
		Expect(err).NotTo(HaveOccurred())
		_, err = reconciler.reconcileRouterPodRecovery(ctx, vnet, true)
		Expect(err).NotTo(HaveOccurred())
		Expect(provider.triggerCount).To(Equal(1))
		Expect(vnet.Status.RouterPodRecovery.State).To(Equal(v1alpha1.JobStateFailed))
		Expect(vnet.Status.RouterPodRecovery.NextRetryTime).NotTo(BeNil())

		result, err := reconciler.reconcileRouterPodRecovery(ctx, vnet, true)
		Expect(err).NotTo(HaveOccurred())
		Expect(result.RequeueAfter).To(BeNumerically(">", 0))
		Expect(provider.triggerCount).To(Equal(1))

		vnet.Status.RouterPodRecovery.NextRetryTime = &metav1.Time{Time: time.Now().Add(-time.Second)}
		_, err = reconciler.reconcileRouterPodRecovery(ctx, vnet, true)
		Expect(err).NotTo(HaveOccurred())
		Expect(provider.triggerCount).To(Equal(2))
	})
})

type routerRecoveryTestProvider struct {
	triggerCount int
	statuses     []provisioning.ProvisionStatus
}

func (p *routerRecoveryTestProvider) TriggerProvision(context.Context, client.Object) (*provisioning.ProvisionResult, error) {
	return &provisioning.ProvisionResult{JobID: "provision", InitialState: v1alpha1.JobStatePending}, nil
}

func (p *routerRecoveryTestProvider) GetProvisionStatus(context.Context, client.Object, string) (provisioning.ProvisionStatus, error) {
	return p.nextRecoveryStatus(), nil
}

func (p *routerRecoveryTestProvider) TriggerDeprovision(context.Context, client.Object, []v1alpha1.JobStatus) (*provisioning.DeprovisionResult, error) {
	return &provisioning.DeprovisionResult{Action: provisioning.DeprovisionTriggered, JobID: "deprovision"}, nil
}

func (p *routerRecoveryTestProvider) GetDeprovisionStatus(context.Context, client.Object, string) (provisioning.ProvisionStatus, error) {
	return provisioning.ProvisionStatus{State: v1alpha1.JobStateSucceeded}, nil
}

func (p *routerRecoveryTestProvider) Name() string { return "router-recovery-test" }

func (p *routerRecoveryTestProvider) TriggerRouterPodRecovery(context.Context, client.Object, string) (*provisioning.ProvisionResult, error) {
	p.triggerCount++
	return &provisioning.ProvisionResult{
		JobID:        "recovery-job",
		InitialState: v1alpha1.JobStatePending,
		Message:      "recovery started",
	}, nil
}

func (p *routerRecoveryTestProvider) nextRecoveryStatus() provisioning.ProvisionStatus {
	if len(p.statuses) == 0 {
		return provisioning.ProvisionStatus{State: v1alpha1.JobStateSucceeded}
	}
	status := p.statuses[0]
	if len(p.statuses) > 1 {
		p.statuses = p.statuses[1:]
	}
	return status
}

func recoveryTestVirtualNetwork() *v1alpha1.VirtualNetwork {
	return &v1alpha1.VirtualNetwork{
		ObjectMeta: metav1.ObjectMeta{Name: "test-vnet", Namespace: "default"},
		Spec: v1alpha1.VirtualNetworkSpec{
			NetworkingType: v1alpha1.VirtualNetworkNetworkingTypeSecondary,
		},
	}
}

func recoveryTestPod(uid string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "router",
			Namespace: "test-vnet",
			UID:       types.UID(uid),
			Labels: map[string]string{
				osacRouterPodLabel:            labelValueTrue,
				osacRouterVirtualNetworkLabel: "test-vnet",
			},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			Conditions: []corev1.PodCondition{{
				Type:   corev1.PodReady,
				Status: corev1.ConditionTrue,
			}},
		},
	}
}

func recoveryTestClient(vnet *v1alpha1.VirtualNetwork, pod *corev1.Pod) client.Client {
	scheme := runtime.NewScheme()
	Expect(v1alpha1.AddToScheme(scheme)).To(Succeed())
	Expect(corev1.AddToScheme(scheme)).To(Succeed())
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(vnet, pod).Build()
}

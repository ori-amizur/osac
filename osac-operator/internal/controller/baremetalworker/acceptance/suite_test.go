/*
Copyright (c) 2026 Red Hat Inc.

Licensed under the Apache License, Version 2.0 (the "License"); you may not use this file except in compliance with the
License. You may obtain a copy of the License at

  http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software distributed under the License is distributed on an
"AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied. See the License for the specific
language governing permissions and limitations under the License.
*/

// Package acceptance holds the bare-metal worker acceptance suite: an envtest harness that wires
// the fake fulfillment-service private API and ignition endpoint (OSAC-4149) and the environment
// simulator (OSAC-4150) together, plus the feature scenarios the controller slices turn green.
// Scenarios that depend on unimplemented slices are marked pending (Ginkgo PIt) so the suite is
// green-with-pending in CI; each is flipped to active as its slice lands.
package acceptance

import (
	"context"
	"fmt"
	"path/filepath"
	"runtime"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	hypershiftv1beta1 "github.com/openshift/hypershift/api/hypershift/v1beta1"
	osacv1alpha1 "github.com/osac-project/osac/osac-operator/api/v1alpha1"
)

var (
	cfg       *rest.Config
	k8sClient client.Client
	testEnv   *envtest.Environment
	ctx       context.Context
	cancel    context.CancelFunc
)

func TestAcceptance(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "BareMetalWorker Acceptance Suite")
}

var _ = BeforeSuite(func() {
	logf.SetLogger(zap.New(zap.WriteTo(GinkgoWriter), zap.UseDevMode(true)))
	ctx, cancel = context.WithCancel(context.TODO())

	testEnv = &envtest.Environment{
		CRDDirectoryPaths: []string{
			filepath.Join("..", "..", "..", "..", "config", "crd", "bases"),
			filepath.Join("..", "..", "..", "..", "config", "crd", "fakes"),
		},
		ErrorIfCRDPathMissing: true,
		BinaryAssetsDirectory: filepath.Join("..", "..", "..", "..", "bin", "k8s",
			fmt.Sprintf("1.31.0-%s-%s", runtime.GOOS, runtime.GOARCH)),
	}

	var err error
	cfg, err = testEnv.Start()
	Expect(err).NotTo(HaveOccurred())
	Expect(cfg).NotTo(BeNil())

	Expect(hypershiftv1beta1.AddToScheme(scheme.Scheme)).To(Succeed())
	Expect(osacv1alpha1.AddToScheme(scheme.Scheme)).To(Succeed())

	k8sClient, err = client.New(cfg, client.Options{Scheme: scheme.Scheme})
	Expect(err).NotTo(HaveOccurred())
	Expect(k8sClient).NotTo(BeNil())

	// Most acceptance scenarios use these tenant-owned network CR references. Keep
	// them in the envtest API server so BMI request construction exercises the same
	// CR-name-to-Fulfillment-ID translation as production.
	Expect(k8sClient.Create(ctx, &osacv1alpha1.Subnet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-subnet",
			Namespace: testNamespace,
			Labels:    map[string]string{"osac.openshift.io/subnet-uuid": "test-subnet-resource-id"},
			Annotations: map[string]string{
				"osac.openshift.io/tenant": "tenant1",
			},
		},
		Spec: osacv1alpha1.SubnetSpec{VirtualNetwork: "test-vnet", IPv4CIDR: "192.0.2.0/24"},
	})).To(Succeed())
	Expect(k8sClient.Create(ctx, &osacv1alpha1.SecurityGroup{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "sg-default",
			Namespace: testNamespace,
			Labels:    map[string]string{"osac.openshift.io/securitygroup-uuid": "test-security-group-resource-id"},
			Annotations: map[string]string{
				"osac.openshift.io/tenant": "tenant1",
			},
		},
		Spec: osacv1alpha1.SecurityGroupSpec{VirtualNetwork: "test-vnet"},
	})).To(Succeed())
})

var _ = AfterSuite(func() {
	cancel()
	Expect(testEnv.Stop()).To(Succeed())
})

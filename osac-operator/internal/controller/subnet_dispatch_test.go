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
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/osac-project/osac/osac-operator/pkg/dispatcher"
	"github.com/osac-project/osac/osac-operator/pkg/networkmanager"
)

var _ = Describe("selectSecondarySubnetDispatchPlan", func() {
	var plan *dispatcher.DispatchPlan

	BeforeEach(func() {
		plan = &dispatcher.DispatchPlan{Targets: []dispatcher.DispatchTarget{
			{Role: dispatcher.ManagerRoleFabric, Manager: networkmanager.Manager{Name: "netris"}},
			{Role: dispatcher.ManagerRoleK8s, Manager: networkmanager.Manager{Name: "cudn-net"}},
		}}
	})

	It("selects the fabric target by default", func() {
		selectedPlan, selectedTarget, err := selectSecondarySubnetDispatchPlan(plan, "")

		Expect(err).NotTo(HaveOccurred())
		Expect(selectedTarget).NotTo(BeNil())
		Expect(selectedTarget.Role).To(Equal(dispatcher.ManagerRoleFabric))
		Expect(selectedTarget.Manager.Name).To(Equal("netris"))
		Expect(selectedPlan.Targets).To(HaveLen(1))
		Expect(selectedPlan.Targets[0].Role).To(Equal(dispatcher.ManagerRoleFabric))
	})

	It("selects the explicitly requested Kubernetes target", func() {
		selectedPlan, selectedTarget, err := selectSecondarySubnetDispatchPlan(plan, "cudn-net")

		Expect(err).NotTo(HaveOccurred())
		Expect(selectedTarget).NotTo(BeNil())
		Expect(selectedTarget.Role).To(Equal(dispatcher.ManagerRoleK8s))
		Expect(selectedTarget.Manager.Name).To(Equal("cudn-net"))
		Expect(selectedPlan.Targets).To(HaveLen(1))
		Expect(selectedPlan.Targets[0].Role).To(Equal(dispatcher.ManagerRoleK8s))
	})

	It("rejects a strategy that is not registered for the parent NetworkClass", func() {
		selectedPlan, selectedTarget, err := selectSecondarySubnetDispatchPlan(plan, "unknown")

		Expect(err).To(MatchError(`implementation strategy "unknown" is not available for the parent NetworkClass`))
		Expect(selectedPlan).To(BeNil())
		Expect(selectedTarget).To(BeNil())
	})

	It("preserves the legacy path when no plan or strategy is available", func() {
		selectedPlan, selectedTarget, err := selectSecondarySubnetDispatchPlan(nil, "")

		Expect(err).NotTo(HaveOccurred())
		Expect(selectedPlan).To(BeNil())
		Expect(selectedTarget).To(BeNil())
	})

	It("rejects an explicit strategy when no dispatch plan is available", func() {
		selectedPlan, selectedTarget, err := selectSecondarySubnetDispatchPlan(nil, "cudn-net")

		Expect(err).To(MatchError("Secondary Subnet requires a resolved dispatch plan"))
		Expect(selectedPlan).To(BeNil())
		Expect(selectedTarget).To(BeNil())
	})
})

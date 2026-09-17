/*
Copyright (c) 2026 Red Hat Inc.

Licensed under the Apache License, Version 2.0 (the "License"); you may not use this file except in compliance with the
License. You may obtain a copy of the License at

  http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software distributed under the License is distributed on an
"AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied. See the License for the specific
language governing permissions and limitations under the License.
*/

package subnet

import (
	. "github.com/onsi/ginkgo/v2/dsl/core"
	. "github.com/onsi/gomega"
)

var _ = Describe("Create subnet command", func() {
	It("defines the implementation strategy flag", func() {
		cmd := Cmd()

		flag := cmd.Flag("implementation-strategy")
		Expect(flag).NotTo(BeNil())
		Expect(flag.DefValue).To(BeEmpty())
	})

	It("includes the implementation strategy in the API spec", func() {
		strategy := "netris"
		runner := &runnerContext{}
		runner.args.implementationStrategy = strategy
		runner.args.ipv4Cidr = "10.0.1.0/24"

		spec := runner.buildSpec("vnet-123")

		Expect(spec.GetVirtualNetwork().GetId()).To(Equal("vnet-123"))
		Expect(spec.GetIpv4Cidr()).To(Equal("10.0.1.0/24"))
		Expect(spec.GetImplementationStrategy()).To(Equal(strategy))
		Expect(spec.HasImplementationStrategy()).To(BeTrue())
	})
})

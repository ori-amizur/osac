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
	"fmt"

	"github.com/osac-project/osac/osac-operator/pkg/dispatcher"
)

// selectSecondarySubnetDispatchPlan reduces a resolved Subnet dispatch plan to
// exactly one backend. The user-facing strategy is a manager name. When it is
// omitted, the fabric target is the default; a k8s target is used only when no
// fabric target exists. This preserves the existing Netris default while still
// allowing an individual Secondary Subnet to select a Kubernetes backend.
//
// Primary Subnets keep the original plan and therefore do not call this helper.
func selectSecondarySubnetDispatchPlan(
	plan *dispatcher.DispatchPlan,
	requestedStrategy string,
) (*dispatcher.DispatchPlan, *dispatcher.DispatchTarget, error) {
	if plan == nil || len(plan.Targets) == 0 {
		// Preserve the pre-dispatcher/legacy path when no strategy was explicitly
		// requested. This is useful for directly-created hub CRs and test fixtures;
		// the normal API path always resolves a plan before provisioning.
		if requestedStrategy == "" {
			return nil, nil, nil
		}
		return nil, nil, fmt.Errorf("Secondary Subnet requires a resolved dispatch plan") //nolint:staticcheck // preserve the existing API error text
	}

	selected := -1
	if requestedStrategy != "" {
		for i := range plan.Targets {
			if plan.Targets[i].Manager.Name == requestedStrategy {
				selected = i
				break
			}
		}
		if selected < 0 {
			return nil, nil, fmt.Errorf(
				"implementation strategy %q is not available for the parent NetworkClass",
				requestedStrategy)
		}
	} else if target := plan.FabricTarget(); target != nil {
		selected = targetIndex(plan, target)
	} else {
		// K8s-only fallback plans contain one K8s target.
		selected = 0
	}

	target := plan.Targets[selected]
	return &dispatcher.DispatchPlan{Targets: []dispatcher.DispatchTarget{target}}, &target, nil
}

func targetIndex(plan *dispatcher.DispatchPlan, target *dispatcher.DispatchTarget) int {
	for i := range plan.Targets {
		if &plan.Targets[i] == target {
			return i
		}
	}
	return -1
}

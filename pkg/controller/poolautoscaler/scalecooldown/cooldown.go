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

package scalecooldown

import (
	"time"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
)

const (
	DefaultScaleUpCooldown = 65 * time.Second
	MinimumScaleUpCooldown = 15 * time.Second
	pendingSafetyMargin    = 5 * time.Second
	maximumPendingTimeout  = 60 * time.Second
)

// ResolveScaleUpCooldown returns the effective cooldown for consecutive scale-up actions.
func ResolveScaleUpCooldown(policy *agentsv1alpha1.CapacityPolicy) time.Duration {
	if policy == nil || policy.ScaleUp == nil || policy.ScaleUp.StabilizationWindowSeconds == nil {
		return DefaultScaleUpCooldown
	}

	configured := time.Duration(*policy.ScaleUp.StabilizationWindowSeconds) * time.Second
	return max(configured, MinimumScaleUpCooldown)
}

// ResolvePendingTimeout returns the Pending timeout derived from the scale-up cooldown.
func ResolvePendingTimeout(policy *agentsv1alpha1.CapacityPolicy) time.Duration {
	return min(ResolveScaleUpCooldown(policy)-pendingSafetyMargin, maximumPendingTimeout)
}

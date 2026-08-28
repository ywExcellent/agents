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
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
)

func TestResolveScaleUpCooldown(t *testing.T) {
	tests := []struct {
		name     string
		policy   *agentsv1alpha1.CapacityPolicy
		expected time.Duration
	}{
		{name: "nil policy uses default", expected: 65 * time.Second},
		{name: "nil scale up uses default", policy: &agentsv1alpha1.CapacityPolicy{}, expected: 65 * time.Second},
		{name: "omitted window uses default", policy: policyWithScaleUpWindow(nil), expected: 65 * time.Second},
		{name: "zero is normalized to minimum", policy: policyWithScaleUpWindow(int32Pointer(0)), expected: 15 * time.Second},
		{name: "below minimum is normalized", policy: policyWithScaleUpWindow(int32Pointer(10)), expected: 15 * time.Second},
		{name: "minimum is unchanged", policy: policyWithScaleUpWindow(int32Pointer(15)), expected: 15 * time.Second},
		{name: "configured value is unchanged", policy: policyWithScaleUpWindow(int32Pointer(120)), expected: 120 * time.Second},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expected, ResolveScaleUpCooldown(tt.policy))
		})
	}
}

func TestResolvePendingTimeout(t *testing.T) {
	tests := []struct {
		name     string
		policy   *agentsv1alpha1.CapacityPolicy
		expected time.Duration
	}{
		{name: "default is sixty seconds", expected: 60 * time.Second},
		{name: "minimum cooldown produces ten seconds", policy: policyWithScaleUpWindow(int32Pointer(0)), expected: 10 * time.Second},
		{name: "thirty second cooldown produces twenty five seconds", policy: policyWithScaleUpWindow(int32Pointer(30)), expected: 25 * time.Second},
		{name: "timeout is capped at sixty seconds", policy: policyWithScaleUpWindow(int32Pointer(120)), expected: 60 * time.Second},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expected, ResolvePendingTimeout(tt.policy))
		})
	}
}

func policyWithScaleUpWindow(window *int32) *agentsv1alpha1.CapacityPolicy {
	return &agentsv1alpha1.CapacityPolicy{
		ScaleUp: &agentsv1alpha1.CapacityScalingRules{StabilizationWindowSeconds: window},
	}
}

func int32Pointer(value int32) *int32 {
	return &value
}

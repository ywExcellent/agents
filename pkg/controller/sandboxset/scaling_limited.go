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

package sandboxset

import (
	"context"
	"fmt"
	"math"
	"time"

	corev1 "k8s.io/api/core/v1"
	apiMeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	intstrutil "k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/controller/poolautoscaler/scalecooldown"
	"github.com/openkruise/agents/pkg/utils"
)

const (
	scalingLimitedReasonBudgetAvailable = "StartupBudgetAvailable"
	scalingLimitedReasonBudgetExhausted = "StartupBudgetExhausted"
	eventScalingLimited                 = "ScalingLimited"
	resourcePendingReason               = "ResourcePending"
)

func (r *Reconciler) calculateScalingLimited(
	ctx context.Context,
	sbs *agentsv1alpha1.SandboxSet,
	status *agentsv1alpha1.SandboxSetStatus,
	groups GroupedSandboxes,
	now time.Time,
) (time.Duration, error) {
	policy, err := r.findCapacityPolicy(ctx, sbs)
	if err != nil {
		return 0, err
	}
	pendingTimeout := scalecooldown.ResolvePendingTimeout(policy)

	failed, timedOut := 0, 0
	var nextDeadline time.Time
	for _, sandbox := range groups.Creating {
		if isStartupFailure(sandbox) {
			failed++
			continue
		}

		state, reason := utils.GetSandboxState(sandbox)
		if state != agentsv1alpha1.SandboxStateCreating || reason != resourcePendingReason {
			continue
		}
		deadline := sandbox.CreationTimestamp.Add(pendingTimeout)
		if !now.Before(deadline) {
			timedOut++
		} else if nextDeadline.IsZero() || deadline.Before(nextDeadline) {
			nextDeadline = deadline
		}
	}

	startupBudget, err := resolveStartupBudget(sbs.Spec.ScaleStrategy.MaxUnavailable, status.Replicas)
	if err != nil {
		return 0, err
	}
	blocked := failed + timedOut
	limited := blocked >= startupBudget
	reason := scalingLimitedReasonBudgetAvailable
	conditionStatus := metav1.ConditionFalse
	if limited {
		reason = scalingLimitedReasonBudgetExhausted
		conditionStatus = metav1.ConditionTrue
	}

	previous := apiMeta.FindStatusCondition(sbs.Status.Conditions, string(agentsv1alpha1.SandboxSetConditionScalingLimited))
	apiMeta.SetStatusCondition(&status.Conditions, metav1.Condition{
		Type:               string(agentsv1alpha1.SandboxSetConditionScalingLimited),
		Status:             conditionStatus,
		ObservedGeneration: sbs.Generation,
		Reason:             reason,
		Message:            fmt.Sprintf("%d of %d startup slots are blocked: Timeout=%d, Failed=%d", blocked, startupBudget, timedOut, failed),
	})
	if limited && (previous == nil || previous.Status != metav1.ConditionTrue) {
		r.Recorder.Eventf(sbs, corev1.EventTypeWarning, eventScalingLimited,
			"SandboxSet startup budget is exhausted: Timeout=%d, Failed=%d, Budget=%d", timedOut, failed, startupBudget)
	}

	if nextDeadline.IsZero() {
		return 0, nil
	}
	return max(nextDeadline.Sub(now), 0), nil
}

func isStartupFailure(sandbox *agentsv1alpha1.Sandbox) bool {
	condition := apiMeta.FindStatusCondition(sandbox.Status.Conditions, string(agentsv1alpha1.SandboxConditionReady))
	return condition != nil && condition.Status == metav1.ConditionFalse &&
		condition.Reason == agentsv1alpha1.SandboxReadyReasonStartContainerFailed
}

func resolveStartupBudget(maxUnavailable *intstrutil.IntOrString, observedReplicas int32) (int, error) {
	executionBase := max(int(observedReplicas), 1)
	if maxUnavailable == nil {
		return executionBase, nil
	}

	resolved, err := intstrutil.GetScaledValueFromIntOrPercent(maxUnavailable, executionBase, true)
	if err != nil {
		return 0, err
	}
	return max(resolved, 1), nil
}

func resolveMaxUnavailable(maxUnavailable *intstrutil.IntOrString, observedReplicas int32) (int, error) {
	if maxUnavailable == nil {
		return math.MaxInt, nil
	}
	resolved, err := intstrutil.GetScaledValueFromIntOrPercent(maxUnavailable, max(int(observedReplicas), 1), true)
	if err != nil {
		return 0, err
	}
	return max(resolved, 0), nil
}

func minimumPositiveDuration(durations ...time.Duration) time.Duration {
	var earliest time.Duration
	for _, duration := range durations {
		if duration > 0 && (earliest == 0 || duration < earliest) {
			earliest = duration
		}
	}
	return earliest
}

func (r *Reconciler) findCapacityPolicy(ctx context.Context, sbs *agentsv1alpha1.SandboxSet) (*agentsv1alpha1.CapacityPolicy, error) {
	if !r.poolAutoscalerAvailable {
		return nil, nil
	}

	list := &agentsv1alpha1.PoolAutoscalerList{}
	if err := r.List(ctx, list, client.InNamespace(sbs.Namespace)); err != nil {
		return nil, fmt.Errorf("list PoolAutoscalers: %w", err)
	}

	var selected *agentsv1alpha1.PoolAutoscaler
	for i := range list.Items {
		candidate := &list.Items[i]
		if candidate.Spec.ScaleTargetRef.Kind != "SandboxSet" || candidate.Spec.ScaleTargetRef.Name != sbs.Name {
			continue
		}
		if selected == nil || candidate.CreationTimestamp.Before(&selected.CreationTimestamp) ||
			(candidate.CreationTimestamp.Equal(&selected.CreationTimestamp) && candidate.Name < selected.Name) {
			selected = candidate
		}
	}
	if selected == nil {
		return nil, nil
	}
	return selected.Spec.CapacityPolicy, nil
}

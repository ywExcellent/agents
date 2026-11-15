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

package poolautoscaler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	api "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/templateindex"
)

var errInactiveManagedTarget = errors.New("managed scaling target is inactive")

// checkManagedTarget uses informer-backed Gets only. A template owner reference
// is a fail-closed managed candidate, not permission to adopt a same-name pool.
// Missing/changed owners and deletion stop scaling after observation, without
// claiming strong coordination with unseen deletes or in-flight API writes.
func (r *Reconciler) checkManagedTarget(ctx context.Context, pa *api.PoolAutoscaler, pool *api.SandboxSet) (bool, error) {
	owner := templateindex.VersionOwnerReference(pa)
	managed := owner != nil
	inactive := func(message string) (bool, error) {
		return managed, fmt.Errorf("%w: %s", errInactiveManagedTarget, message)
	}
	if pa.DeletionTimestamp != nil || pool.DeletionTimestamp != nil {
		return inactive("autoscaler or pool is deleting")
	}
	if !managed {
		return false, nil
	}
	v := &api.SandboxTemplate{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: pa.Namespace, Name: owner.Name}, v); err != nil {
		if client.IgnoreNotFound(err) == nil {
			return inactive("version is not observed")
		}
		return true, err
	}
	if v.UID != owner.UID || v.DeletionTimestamp != nil {
		return inactive("version UID changed or version is deleting")
	}
	claimOwner := templateindex.ClaimOwnerReference(v)
	if claimOwner == nil {
		return inactive("version has no valid claim owner")
	}
	claim := &api.SandboxTemplateClaim{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: v.Namespace, Name: claimOwner.Name}, claim); err != nil {
		if client.IgnoreNotFound(err) == nil {
			return inactive("claim is not observed")
		}
		return true, err
	}
	if claim.UID != claimOwner.UID || claim.DeletionTimestamp != nil {
		return inactive("claim UID changed or claim is deleting")
	}
	poolOwner := templateindex.VersionOwnerReference(pool)
	if pool.Namespace != v.Namespace || poolOwner == nil || poolOwner.Name != v.Name || poolOwner.UID != v.UID {
		return inactive("pool does not share the actual version owner")
	}
	if pa.Name != v.Name || pool.Name != v.Name || pa.Spec.ScaleTargetRef.Name != v.Name || pa.Spec.ScaleTargetRef.Kind != "SandboxSet" || pa.Spec.ScaleTargetRef.APIVersion != api.GroupVersion.String() {
		return inactive("managed target reference differs from version")
	}
	ref := pool.Spec.TemplateRef
	if pool.Spec.Template != nil || ref == nil || ref.Name != v.Name || ptr.Deref(ref.Kind, "") != "SandboxTemplate" || ptr.Deref(ref.APIVersion, "") != api.GroupVersion.String() {
		return inactive("pool definition reference differs from version")
	}
	if v.Spec.BuildOptions == nil || v.Spec.BuildOptions.WarmPool == nil || !ptr.Deref(v.Spec.BuildOptions.WarmPool.Enabled, true) || v.Spec.BuildOptions.WarmPool.Autoscaler == nil {
		return inactive("version does not declare automatic mode")
	}
	return true, nil
}

// scaleTarget rechecks cached identities immediately before managed writes, then
// atomically tests pool UID/RV in the API server. The PA/owner reads are not a
// transaction: cache propagation and already submitted scaling are not drained.
// Unmanaged autoscalers retain the existing scaling algorithm and write path.
func (r *Reconciler) scaleTarget(ctx context.Context, pa *api.PoolAutoscaler, pool *api.SandboxSet, desired int32, managed bool) error {
	if !managed {
		return r.doScale(ctx, pool, desired)
	}
	current := &api.PoolAutoscaler{}
	if err := r.Get(ctx, client.ObjectKeyFromObject(pa), current); err != nil {
		return err
	}
	if current.UID != pa.UID || current.ResourceVersion != pa.ResourceVersion {
		return fmt.Errorf("%w: autoscaler snapshot changed", errInactiveManagedTarget)
	}
	target := &api.SandboxSet{}
	if err := r.Get(ctx, client.ObjectKeyFromObject(pool), target); err != nil {
		return err
	}
	if target.UID != pool.UID || target.ResourceVersion != pool.ResourceVersion {
		return fmt.Errorf("%w: pool snapshot changed", errInactiveManagedTarget)
	}
	if _, err := r.checkManagedTarget(ctx, current, target); err != nil {
		return err
	}
	patch, err := json.Marshal([]map[string]any{
		{"op": "test", "path": "/metadata/uid", "value": pool.UID},
		{"op": "test", "path": "/metadata/resourceVersion", "value": pool.ResourceVersion},
		{"op": "replace", "path": "/spec/replicas", "value": desired},
	})
	if err != nil {
		return err
	}
	return r.Patch(ctx, pool, client.RawPatch(types.JSONPatchType, patch))
}

// Owner changes wake managed autoscalers, including a Claim delete that has not
// yet propagated to its version. These cached namespace reads add no live Lists.
func (r *Reconciler) templateToPoolAutoscaler(ctx context.Context, obj client.Object) []ctrl.Request {
	list := &api.PoolAutoscalerList{}
	if err := r.List(ctx, list, client.InNamespace(obj.GetNamespace()), client.MatchingFields{scaleTargetRefNameIndex: obj.GetName()}); err != nil {
		log.FromContext(ctx).Error(err, "list autoscalers for template event")
		return nil
	}
	out := []ctrl.Request{}
	for i := range list.Items {
		if o := templateindex.VersionOwnerReference(&list.Items[i]); o != nil && o.Name == obj.GetName() {
			out = append(out, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(&list.Items[i])})
		}
	}
	return out
}
func (r *Reconciler) claimToPoolAutoscaler(ctx context.Context, obj client.Object) []ctrl.Request {
	list := &api.PoolAutoscalerList{}
	if err := r.List(ctx, list, client.InNamespace(obj.GetNamespace())); err != nil {
		log.FromContext(ctx).Error(err, "list autoscalers for claim event")
		return nil
	}
	out := []ctrl.Request{}
	for i := range list.Items {
		o := templateindex.VersionOwnerReference(&list.Items[i])
		if o == nil {
			continue
		}
		v := &api.SandboxTemplate{}
		if err := r.Get(ctx, client.ObjectKey{Namespace: obj.GetNamespace(), Name: o.Name}, v); err != nil {
			if client.IgnoreNotFound(err) != nil {
				log.FromContext(ctx).Error(err, "get version for claim event")
			}
			continue
		}
		co := templateindex.ClaimOwnerReference(v)
		if co != nil && co.Name == obj.GetName() {
			out = append(out, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(&list.Items[i])})
		}
	}
	return out
}

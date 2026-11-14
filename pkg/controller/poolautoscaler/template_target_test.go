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
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	api "github.com/openkruise/agents/api/v1alpha1"
)

func managedFixture() (*api.SandboxTemplateClaim, *api.SandboxTemplate, *api.PoolAutoscaler, *api.SandboxSet) {
	c := &api.SandboxTemplateClaim{ObjectMeta: metav1.ObjectMeta{Name: "demo", Namespace: "team", UID: "claim", Finalizers: []string{"test/hold"}}}
	owner := func(kind, name string, uid types.UID) metav1.OwnerReference {
		return metav1.OwnerReference{APIVersion: api.GroupVersion.String(), Kind: kind, Name: name, UID: uid, Controller: ptr.To(true)}
	}
	v := &api.SandboxTemplate{ObjectMeta: metav1.ObjectMeta{Name: "abcdefghijklmnopqrst", Namespace: c.Namespace, UID: "version", Finalizers: []string{"test/hold"}, OwnerReferences: []metav1.OwnerReference{owner("SandboxTemplateClaim", c.Name, c.UID)}}, Spec: api.SandboxTemplateSpec{BuildOptions: &api.SandboxTemplateBuildOptions{WarmPool: &api.SandboxTemplateWarmPoolOptions{Enabled: ptr.To(true), Autoscaler: &api.SandboxTemplatePoolAutoscalerOptions{MinReplicas: 2, MaxReplicas: 10, CapacityPolicy: &api.CapacityPolicy{TargetAvailable: intstr.FromInt32(2)}}}}}}
	pa := newPoolAutoscaler(v.Name, v.Namespace, v.Name, 10, &api.CapacityPolicy{TargetAvailable: intstr.FromInt32(2)})
	pa.UID = "autoscaler"
	pa.Spec.MinReplicas = 2
	pa.Spec.ScaleTargetRef.APIVersion = api.GroupVersion.String()
	pa.OwnerReferences = []metav1.OwnerReference{owner("SandboxTemplate", v.Name, v.UID)}
	pa.Finalizers = []string{"test/hold"}
	p := newSandboxSet(v.Name, v.Namespace, 0, 0, 0)
	p.UID = "pool"
	p.Finalizers = []string{"test/hold"}
	p.OwnerReferences = []metav1.OwnerReference{owner("SandboxTemplate", v.Name, v.UID)}
	p.Spec.TemplateRef = &api.SandboxTemplateRef{Name: v.Name, Kind: ptr.To("SandboxTemplate"), APIVersion: ptr.To(api.GroupVersion.String())}
	return c, v, pa, p
}
func TestManagedAutoscalerStopsOnObservedDeletionOrOwnerMismatch(t *testing.T) {
	for _, scenario := range []string{"active", "claim-delete", "version-delete", "pool-delete", "pa-delete", "claim-uid", "version-uid", "pool-owner", "target-name", "no-auto", "claim-missing"} {
		t.Run(scenario, func(t *testing.T) {
			c, v, pa, p := managedFixture()
			now := metav1.Now()
			switch scenario {
			case "claim-delete":
				c.DeletionTimestamp = &now
			case "version-delete":
				v.DeletionTimestamp = &now
			case "pool-delete":
				p.DeletionTimestamp = &now
			case "pa-delete":
				pa.DeletionTimestamp = &now
			case "claim-uid":
				c.UID = "new-claim"
			case "version-uid":
				v.UID = "new-version"
			case "pool-owner":
				p.OwnerReferences[0].UID = "foreign"
			case "target-name":
				pa.Spec.ScaleTargetRef.Kind = "Other"
			case "no-auto":
				v.Spec.BuildOptions.WarmPool.Autoscaler = nil
			}
			objs := []client.Object{v, pa, p}
			if scenario != "claim-missing" {
				objs = append(objs, c)
			}
			r := newTestReconciler(objs...)
			if _, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(pa)}); err != nil {
				t.Fatal(err)
			}
			got := &api.SandboxSet{}
			if err := r.Get(t.Context(), client.ObjectKeyFromObject(p), got); err != nil {
				t.Fatal(err)
			}
			want := int32(0)
			if scenario == "active" {
				want = 2
			}
			if got.Spec.Replicas != want {
				t.Fatalf("got %d want %d", got.Spec.Replicas, want)
			}
		})
	}
}
func TestManagedTargetMonitorResetsOnPoolUID(t *testing.T) {
	c, v, pa, p := managedFixture()
	r := newTestReconciler(c, v, pa, p)
	key := client.ObjectKeyFromObject(pa)
	old := r.getOrCreateMonitorFor(key, pa.UID, pa.Spec.ScaleTargetRef.Name, p.UID)
	old.samples = []sample{{timestamp: time.Now(), available: 99, statusReplicas: 99}}
	old.lastScaleUpAt = time.Now()
	old.lastScaleDownAt = time.Now()
	p.UID = "rebuilt-pool"
	// Production observation and stabilization both carry the actual target UID.
	avail, _, _ := r.observeAndAggregate(t.Context(), pa, 3, 4, p.UID)
	current := r.getOrCreateMonitorFor(key, pa.UID, pa.Spec.ScaleTargetRef.Name, p.UID)
	if current == old || avail != 3 || !current.lastScaleUpAt.IsZero() || !current.lastScaleDownAt.IsZero() {
		t.Fatal("old target history leaked")
	}
	if _, blocked, _ := r.applyStabilizationWindow(pa, 2, 3, p.UID); blocked {
		t.Fatal("old target cooldown leaked")
	}
	if r.getOrCreateMonitorFor(key, pa.UID, pa.Spec.ScaleTargetRef.Name, p.UID) != current {
		t.Fatal("same target reset")
	}
}
func TestManagedScaleRechecksCachedOwnership(t *testing.T) {
	c, v, pa, p := managedFixture()
	r := newTestReconciler(c, v, pa, p)
	if err := r.Get(t.Context(), client.ObjectKeyFromObject(pa), pa); err != nil {
		t.Fatal(err)
	}
	if err := r.Get(t.Context(), client.ObjectKeyFromObject(p), p); err != nil {
		t.Fatal(err)
	}
	if err := r.Delete(t.Context(), c); err != nil {
		t.Fatal(err)
	}
	if err := r.scaleTarget(t.Context(), pa, p, 5, true); !errors.Is(err, errInactiveManagedTarget) {
		t.Fatalf("expected stopped write: %v", err)
	}
	got := &api.SandboxSet{}
	if err := r.Get(t.Context(), client.ObjectKeyFromObject(p), got); err != nil || got.Spec.Replicas != 0 {
		t.Fatal("scaled after observed delete", err)
	}
}
func TestManagedScalePatchBindsUIDAndRVAndRejectsLateReplacement(t *testing.T) {
	c, v, pa, p := managedFixture()
	r := newTestReconciler(c, v, pa, p)
	base := r.Client.(client.WithWatch)
	if err := base.Get(t.Context(), client.ObjectKeyFromObject(pa), pa); err != nil {
		t.Fatal(err)
	}
	if err := base.Get(t.Context(), client.ObjectKeyFromObject(p), p); err != nil {
		t.Fatal(err)
	}
	r.Client = interceptor.NewClient(base, interceptor.Funcs{Patch: func(ctx context.Context, cl client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
		data, err := patch.Data(obj)
		if err != nil {
			return err
		}
		var ops []map[string]any
		if err := json.Unmarshal(data, &ops); err != nil {
			return err
		}
		if patch.Type() != types.JSONPatchType || ops[0]["value"] != string(p.UID) || ops[1]["value"] != p.ResourceVersion {
			t.Fatalf("patch=%s", data)
		}
		if err := cl.Delete(ctx, p); err != nil {
			return err
		}
		// Simulate deletion completion followed by replacement after the cached read.
		old := &api.SandboxSet{}
		if err := cl.Get(ctx, client.ObjectKeyFromObject(p), old); err != nil {
			return err
		}
		old.Finalizers = nil
		if err := cl.Update(ctx, old); err != nil {
			return err
		}
		replacement := p.DeepCopy()
		replacement.UID = "replacement"
		replacement.ResourceVersion = ""
		replacement.Finalizers = nil
		replacement.DeletionTimestamp = nil
		if err := cl.Create(ctx, replacement); err != nil {
			return err
		}
		return cl.Patch(ctx, obj, patch, opts...)
	}})
	if err := r.scaleTarget(t.Context(), pa, p, 5, true); err == nil {
		t.Fatal("late replacement accepted stale patch")
	}
	got := &api.SandboxSet{}
	if err := base.Get(t.Context(), client.ObjectKeyFromObject(p), got); err != nil || got.UID != "replacement" || got.Spec.Replicas != 0 {
		t.Fatal("replacement changed", err)
	}
}
func TestManagedOwnerEventsWakeAutoscaler(t *testing.T) {
	c, v, pa, p := managedFixture()
	r := newTestReconciler(c, v, pa, p)
	if got := r.templateToPoolAutoscaler(t.Context(), v); len(got) != 1 || got[0].NamespacedName != client.ObjectKeyFromObject(pa) {
		t.Fatalf("template events=%v", got)
	}
	if got := r.claimToPoolAutoscaler(t.Context(), c); len(got) != 1 || got[0].NamespacedName != client.ObjectKeyFromObject(pa) {
		t.Fatalf("claim events=%v", got)
	}
}

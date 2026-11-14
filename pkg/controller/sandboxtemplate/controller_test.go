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
package sandboxtemplate

import (
	"context"
	"fmt"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/tools/events"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	api "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/lifecycle/pool"
	"github.com/openkruise/agents/pkg/templatecreation"
	"github.com/openkruise/agents/pkg/templateindex"
)

const testVersionID = "a7f2k9m4p1q8r3s6t0v5"

func fixture() (*api.SandboxTemplateClaim, *api.SandboxTemplate) {
	c := &api.SandboxTemplateClaim{ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "python-agent", UID: "claim-uid", Finalizers: []string{api.SandboxTemplateClaimCleanupFinalizer}, Annotations: map[string]string{templatecreation.AllocationAnnotation: testVersionID, templatecreation.CreationCompleteAnnotation: "true"}}}
	v := &api.SandboxTemplate{ObjectMeta: metav1.ObjectMeta{Namespace: c.Namespace, Name: testVersionID, UID: "version-uid", Generation: 1, Finalizers: []string{api.SandboxTemplateCleanupFinalizer}, OwnerReferences: []metav1.OwnerReference{{APIVersion: api.GroupVersion.String(), Kind: "SandboxTemplateClaim", Name: c.Name, UID: c.UID, Controller: ptr.To(true), BlockOwnerDeletion: ptr.To(true)}}}, Spec: api.SandboxTemplateSpec{Backend: "k8s", BuildOptions: &api.SandboxTemplateBuildOptions{WarmPool: &api.SandboxTemplateWarmPoolOptions{Enabled: ptr.To(true), Size: ptr.To(int32(2))}}, Template: &corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "main", Image: "python:3.12"}}}}, Runtimes: []api.RuntimeConfig{{Name: "agent-runtime"}}}}
	return c, v
}
func fixtureClient(t *testing.T, objects ...client.Object) (client.WithWatch, *runtime.Scheme) {
	t.Helper()
	s := runtime.NewScheme()
	if err := api.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(objects...).WithStatusSubresource(&api.SandboxTemplate{}).
		WithIndex(&api.SandboxTemplate{}, templateindex.ClaimOwner, templateindex.ClaimOwnerUID).
		WithIndex(&api.SandboxSet{}, templateindex.VersionOwner, templateindex.VersionOwnerUID).
		WithIndex(&api.PoolAutoscaler{}, templateindex.VersionOwner, templateindex.VersionOwnerUID).Build()
	return c, s
}
func TestTemplateDefinitionAndFixedPool(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*api.SandboxTemplate)
		size   int32
		pool   bool
		status api.SandboxTemplateBuildPhase
	}{
		{name: "fixed", size: 2, pool: true, status: api.SandboxTemplatePhaseReady},
		{name: "default zero", mutate: func(v *api.SandboxTemplate) { v.Spec.BuildOptions.WarmPool.Size = nil }, pool: true, status: api.SandboxTemplatePhaseReady},
		{name: "disabled", mutate: func(v *api.SandboxTemplate) {
			v.Spec.BuildOptions.WarmPool.Enabled = ptr.To(false)
			v.Spec.BuildOptions.WarmPool.Size = nil
		}, status: api.SandboxTemplatePhaseReady},
		{name: "invalid definition", mutate: func(v *api.SandboxTemplate) { v.Spec.Template.Spec.Containers = nil }, status: api.SandboxTemplatePhaseFailed},
		{name: "legacy skipped", mutate: func(v *api.SandboxTemplate) { v.OwnerReferences = nil }},
	} {
		t.Run(test.name, func(t *testing.T) {
			c, v := fixture()
			if test.mutate != nil {
				test.mutate(v)
			}
			backend, scheme := fixtureClient(t, c, v)
			r := &Reconciler{Client: backend, Reader: backend, Scheme: scheme}
			_, err := r.ReconcileVersion(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(v)})
			if err != nil {
				t.Fatal(err)
			}
			current := &api.SandboxTemplate{}
			if err := backend.Get(t.Context(), client.ObjectKeyFromObject(v), current); err != nil {
				t.Fatal(err)
			}
			if current.Status.Phase != test.status {
				t.Fatalf("phase=%s", current.Status.Phase)
			}
			p := &api.SandboxSet{}
			err = backend.Get(t.Context(), client.ObjectKeyFromObject(v), p)
			if test.pool {
				if err != nil || p.Spec.Replicas != test.size || p.Spec.Template != nil || !ownedByVersion(p, v) {
					t.Fatalf("pool=%+v error=%v", p, err)
				}
				p.UID = "pool-uid"
				sandbox, err := pool.NewSandboxFromSandboxSet(p, current, current)
				if err != nil || len(sandbox.Spec.Runtimes) != 1 || len(sandbox.OwnerReferences) != 1 || *sandbox.OwnerReferences[0].Controller || !*sandbox.OwnerReferences[0].BlockOwnerDeletion {
					t.Fatalf("materialization=%+v error=%v", sandbox, err)
				}
			} else if !apierrors.IsNotFound(err) {
				t.Fatalf("pool unexpectedly present: %v", err)
			}
		})
	}
}
func TestTemplateCompanionCollisionKeepsReady(t *testing.T) {
	c, v := fixture()
	external := &api.SandboxSet{ObjectMeta: metav1.ObjectMeta{Namespace: v.Namespace, Name: v.Name, UID: "external"}}
	backend, scheme := fixtureClient(t, c, v, external)
	recorder := events.NewFakeRecorder(2)
	r := &Reconciler{Client: backend, Reader: backend, Scheme: scheme, Recorder: recorder}
	if _, err := r.ReconcileVersion(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(v)}); err == nil {
		t.Fatal("collision must be reported")
	}
	current := &api.SandboxTemplate{}
	if err := backend.Get(t.Context(), client.ObjectKeyFromObject(v), current); err != nil {
		t.Fatal(err)
	}
	if current.Status.Phase != api.SandboxTemplatePhaseReady || !meta.IsStatusConditionTrue(current.Status.Conditions, templatecreation.DefinitionCondition) {
		t.Fatalf("phase=%s", current.Status.Phase)
	}
	p := &api.SandboxSet{}
	if err := backend.Get(t.Context(), client.ObjectKeyFromObject(external), p); err != nil || p.UID != "external" || len(p.OwnerReferences) != 0 {
		t.Fatal("external pool changed")
	}
	select {
	case <-recorder.Events:
	default:
		t.Fatal("failure must be visible on version")
	}
}
func TestTemplateAutomaticIntentDoesNotResetReplicas(t *testing.T) {
	c, v := fixture()
	v.Spec.BuildOptions.WarmPool.Autoscaler = &api.SandboxTemplatePoolAutoscalerOptions{MinReplicas: 1, MaxReplicas: 10, CapacityPolicy: &api.CapacityPolicy{TargetAvailable: intstr.FromInt32(2)}}
	backend, scheme := fixtureClient(t, c, v)
	r := &Reconciler{Client: backend, Reader: backend, Scheme: scheme}
	req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(v)}
	if _, err := r.ReconcileVersion(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	p := &api.SandboxSet{}
	if err := backend.Get(t.Context(), req.NamespacedName, p); err != nil {
		t.Fatal(err)
	}
	p.Spec.Replicas = 7
	if err := backend.Update(t.Context(), p); err != nil {
		t.Fatal(err)
	}
	if _, err := r.ReconcileVersion(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	if err := backend.Get(t.Context(), req.NamespacedName, p); err != nil || p.Spec.Replicas != 7 {
		t.Fatalf("replicas=%d error=%v", p.Spec.Replicas, err)
	}
}

// The fake models API persistence only. We explicitly deliver GC completion
// events below; this is an ordering/finalizer test, not real GC validation.
func TestTemplateCleanupOrderingAndDurability(t *testing.T) {
	c, v := fixture()
	base, scheme := fixtureClient(t, c, v)
	r := &Reconciler{Client: base, Reader: base, Scheme: scheme}
	req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(v)}
	if _, err := r.ReconcileVersion(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	pa := &api.PoolAutoscaler{ObjectMeta: metav1.ObjectMeta{Namespace: v.Namespace, Name: v.Name, UID: "autoscaler-uid", Finalizers: []string{"test/pending-autoscaler"}}}
	if err := controllerutil.SetControllerReference(v, pa, scheme); err != nil {
		t.Fatal(err)
	}
	if err := base.Create(t.Context(), pa); err != nil {
		t.Fatal(err)
	}
	writes := []string{}
	writer := interceptor.NewClient(base, interceptor.Funcs{Delete: func(ctx context.Context, cl client.WithWatch, o client.Object, opts ...client.DeleteOption) error {
		writes = append(writes, fmt.Sprintf("%T", o))
		options := (&client.DeleteOptions{}).ApplyOptions(opts)
		if options.PropagationPolicy != nil && *options.PropagationPolicy == metav1.DeletePropagationForeground {
			current := o.DeepCopyObject().(client.Object)
			if err := cl.Get(ctx, client.ObjectKeyFromObject(o), current); err != nil {
				return err
			}
			if options.Preconditions != nil && options.Preconditions.ResourceVersion != nil && *options.Preconditions.ResourceVersion != current.GetResourceVersion() {
				t.Fatal("caller supplied a stale delete snapshot")
			}
			if !controllerutil.ContainsFinalizer(current, metav1.FinalizerDeleteDependents) {
				controllerutil.AddFinalizer(current, metav1.FinalizerDeleteDependents)
				if err := cl.Update(ctx, current); err != nil {
					return err
				}
			}
			// The real API server checks preconditions before atomically adding the
			// GC finalizer. The fake needed a separate Update above, so use that
			// updated RV only after verifying the original precondition.
			adjusted := *options
			adjusted.Preconditions = &metav1.Preconditions{UID: ptr.To(current.GetUID()), ResourceVersion: ptr.To(current.GetResourceVersion())}
			return cl.Delete(ctx, current, &adjusted)
		}
		return cl.Delete(ctx, o, opts...)
	}})
	r.Client = writer
	if err := base.Delete(t.Context(), c); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if _, err := r.ReconcileVersion(t.Context(), req); err != nil {
			t.Fatal(err)
		}
	}
	p := &api.SandboxSet{}
	if err := base.Get(t.Context(), req.NamespacedName, p); err != nil || p.DeletionTimestamp != nil {
		t.Fatal("pool removed before autoscaler completed")
	}
	finish := func(o client.Object) {
		t.Helper()
		if err := base.Get(t.Context(), client.ObjectKeyFromObject(o), o); err != nil {
			t.Fatal(err)
		}
		o.SetFinalizers(nil)
		if err := base.Update(t.Context(), o); err != nil {
			t.Fatal(err)
		}
	}
	finish(pa)
	if _, err := r.ReconcileVersion(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	if err := base.Get(t.Context(), req.NamespacedName, p); err != nil || p.DeletionTimestamp == nil {
		t.Fatal("pool cleanup not triggered")
	}
	if _, err := r.ReconcileClaim(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(c)}); err != nil {
		t.Fatal(err)
	}
	held := &api.SandboxTemplateClaim{}
	if err := base.Get(t.Context(), client.ObjectKeyFromObject(c), held); err != nil {
		t.Fatal("Claim released while descendants pending")
	}
	finish(p)
	// Reconstruct the reconciler as a process-restart simulation. No request
	// memory is used to discover the remaining version/GC barrier.
	r = &Reconciler{Client: writer, Reader: base, Scheme: scheme}
	for i := 0; i < 2; i++ {
		if _, err := r.ReconcileVersion(t.Context(), req); err != nil {
			t.Fatal(err)
		}
	}
	current := &api.SandboxTemplate{}
	if err := base.Get(t.Context(), req.NamespacedName, current); err != nil {
		t.Fatal(err)
	}
	if !controllerutil.ContainsFinalizer(current, metav1.FinalizerDeleteDependents) || controllerutil.ContainsFinalizer(current, api.SandboxTemplateCleanupFinalizer) {
		t.Fatalf("finalizers=%v", current.Finalizers)
	}
	if _, err := r.ReconcileClaim(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(c)}); err != nil {
		t.Fatal(err)
	}
	if err := base.Get(t.Context(), client.ObjectKeyFromObject(c), held); err != nil {
		t.Fatal("Claim released before foreground inventory GC")
	}
	finish(current)
	if _, err := r.ReconcileClaim(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(c)}); err != nil {
		t.Fatal(err)
	}
	if err := base.Get(t.Context(), client.ObjectKeyFromObject(c), held); !apierrors.IsNotFound(err) {
		t.Fatalf("claim not removed: %v", err)
	}
	if len(writes) < 4 {
		t.Fatalf("cleanup writes=%v", writes)
	}
}
func TestTemplateEmptyClaimCreatorExitGrace(t *testing.T) {
	for _, expired := range []bool{false, true} {
		t.Run(fmt.Sprintf("expired=%v", expired), func(t *testing.T) {
			c, _ := fixture()
			delete(c.Annotations, templatecreation.CreationCompleteAnnotation)
			c.CreationTimestamp = metav1.NewTime(time.Now())
			if expired {
				c.CreationTimestamp = metav1.NewTime(time.Now().Add(-2 * templatecreation.EmptyCreationGrace))
			}
			backend, scheme := fixtureClient(t, c)
			if err := backend.Delete(t.Context(), c); err != nil {
				t.Fatal(err)
			}
			r := &Reconciler{Client: backend, Reader: backend, Scheme: scheme}
			result, err := r.ReconcileClaim(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(c)})
			if err != nil {
				t.Fatal(err)
			}
			held := &api.SandboxTemplateClaim{}
			err = backend.Get(t.Context(), client.ObjectKeyFromObject(c), held)
			if expired && !apierrors.IsNotFound(err) {
				t.Fatalf("crashed empty creation did not clean: %v", err)
			}
			if !expired && (err != nil || result.RequeueAfter <= 0) {
				t.Fatal("in-flight creator lost its grace")
			}
		})
	}
}

func TestTemplateCleanupCompletedForegroundDoesNotRearmGC(t *testing.T) {
	c, v := fixture()
	v.Annotations = map[string]string{templatecreation.CleanupForegroundAnnotation: "true"}
	// This is the durable state after zero-stock GC already removed its own
	// foregroundDeletion finalizer, while the custom cleanup finalizer remains.
	backend, scheme := fixtureClient(t, c, v)
	if err := backend.Delete(t.Context(), v); err != nil {
		t.Fatal(err)
	}
	r := &Reconciler{Client: backend, Reader: backend, Scheme: scheme}
	if _, err := r.ReconcileVersion(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(v)}); err != nil {
		t.Fatal(err)
	}
	current := &api.SandboxTemplate{}
	if err := backend.Get(t.Context(), client.ObjectKeyFromObject(v), current); !apierrors.IsNotFound(err) {
		t.Fatalf("completed GC was unnecessarily rearmed: %v", err)
	}
}

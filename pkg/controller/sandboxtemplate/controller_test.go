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
	v := &api.SandboxTemplate{ObjectMeta: metav1.ObjectMeta{Namespace: c.Namespace, Name: testVersionID, UID: "version-uid", Generation: 1, OwnerReferences: []metav1.OwnerReference{{APIVersion: api.GroupVersion.String(), Kind: "SandboxTemplateClaim", Name: c.Name, UID: c.UID, Controller: ptr.To(true), BlockOwnerDeletion: ptr.To(true)}}}, Spec: api.SandboxTemplateSpec{Backend: "k8s", BuildOptions: &api.SandboxTemplateBuildOptions{WarmPool: &api.SandboxTemplateWarmPoolOptions{Enabled: ptr.To(true), Size: ptr.To(int32(2))}}, Template: &corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "main", Image: "python:3.12"}}}}, Runtimes: []api.RuntimeConfig{{Name: "agent-runtime"}}}}
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
		WithIndex(&api.SandboxTemplate{}, templateindex.ClaimOwner, templateindex.ClaimOwnerUID).Build()
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

// The fake models foreground-finalizer persistence, not garbage collection.
func TestTemplateForegroundCleanupHoldsClaim(t *testing.T) {
	for _, initiator := range []string{"claim", "version"} {
		t.Run(initiator, func(t *testing.T) {
			c, v := fixture()
			base, scheme := fixtureClient(t, c, v)
			deletes := 0
			writer := interceptor.NewClient(base, interceptor.Funcs{Delete: func(ctx context.Context, cl client.WithWatch, o client.Object, opts ...client.DeleteOption) error {
				deletes++
				options := (&client.DeleteOptions{}).ApplyOptions(opts)
				if _, ok := o.(*api.SandboxTemplate); !ok || ptr.Deref(options.PropagationPolicy, "") != metav1.DeletePropagationForeground {
					t.Fatal("cleanup must request foreground deletion of the version only")
				}
				current := &api.SandboxTemplate{}
				if err := cl.Get(ctx, client.ObjectKeyFromObject(o), current); err != nil {
					return err
				}
				if options.Preconditions == nil || ptr.Deref(options.Preconditions.UID, "") != current.UID || ptr.Deref(options.Preconditions.ResourceVersion, "") != current.ResourceVersion {
					t.Fatal("foreground deletion must pin UID and resourceVersion")
				}
				controllerutil.AddFinalizer(current, metav1.FinalizerDeleteDependents)
				if err := cl.Update(ctx, current); err != nil {
					return err
				}
				// The fake needs a separate Update to model the API server's atomic GC finalizer.
				options.Preconditions.ResourceVersion = &current.ResourceVersion
				return cl.Delete(ctx, current, options)
			}})
			if err := base.Delete(t.Context(), c); err != nil {
				t.Fatal(err)
			}
			r := &Reconciler{Client: writer, Reader: base, Scheme: scheme}
			claimReq := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(c)}
			if initiator == "claim" {
				if _, err := r.ReconcileClaim(t.Context(), claimReq); err != nil {
					t.Fatal(err)
				}
			} else if _, err := r.ReconcileVersion(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(v)}); err != nil {
				t.Fatal(err)
			}
			r = &Reconciler{Client: writer, Reader: base, Scheme: scheme}
			if _, err := r.ReconcileClaim(t.Context(), claimReq); err != nil {
				t.Fatal(err)
			}
			held := &api.SandboxTemplateClaim{}
			if err := base.Get(t.Context(), claimReq.NamespacedName, held); err != nil {
				t.Fatal("Claim released before foreground GC completed", err)
			}
			current := &api.SandboxTemplate{}
			if err := base.Get(t.Context(), client.ObjectKeyFromObject(v), current); err != nil {
				t.Fatal(err)
			}
			if current.DeletionTimestamp == nil || !controllerutil.ContainsFinalizer(current, metav1.FinalizerDeleteDependents) || deletes != 1 {
				t.Fatalf("version=%+v deletes=%d", current, deletes)
			}
			controllerutil.RemoveFinalizer(current, metav1.FinalizerDeleteDependents)
			if err := base.Update(t.Context(), current); err != nil {
				t.Fatal(err)
			}
			if _, err := r.ReconcileClaim(t.Context(), claimReq); err != nil {
				t.Fatal(err)
			}
			if err := base.Get(t.Context(), claimReq.NamespacedName, held); !apierrors.IsNotFound(err) {
				t.Fatalf("Claim not removed after GC: %v", err)
			}
		})
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

func TestTemplateVersionConfirmsOnlyNegativeClaimObservations(t *testing.T) {
	for _, state := range []string{"current", "missing", "old UID"} {
		t.Run(state, func(t *testing.T) {
			c, v := fixture()
			objects := []client.Object{v}
			if state != "missing" {
				cachedClaim := c.DeepCopy()
				if state == "old UID" {
					cachedClaim.UID = "old-claim"
				}
				objects = append(objects, cachedClaim)
			}
			cached, scheme := fixtureClient(t, objects...)
			live, _ := fixtureClient(t, c)
			reads := 0
			reader := interceptor.NewClient(live, interceptor.Funcs{Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				reads++
				return cl.Get(ctx, key, obj, opts...)
			}})
			r := &Reconciler{Client: cached, Reader: reader, Scheme: scheme}
			result, err := r.ReconcileVersion(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(v)})
			if err != nil || result.RequeueAfter != 0 {
				t.Fatalf("result=%+v error=%v", result, err)
			}
			want := 1
			if state == "current" {
				want = 0
			}
			if reads != want {
				t.Fatalf("live reads=%d want=%d", reads, want)
			}
		})
	}
}

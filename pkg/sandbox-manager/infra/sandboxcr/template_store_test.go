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

package sandboxcr

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrlcache "sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	api "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
	"github.com/openkruise/agents/pkg/templateconfig"
	"github.com/openkruise/agents/pkg/templatecreation"
	"github.com/openkruise/agents/pkg/templateindex"
)

const templateTestID = "a7f2k9m4p1q8r3s6t0v5"

func templateTestFixture() (*api.SandboxTemplateClaim, *api.SandboxTemplate) {
	c := &api.SandboxTemplateClaim{ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "python-agent", UID: "claim-uid", Finalizers: []string{api.SandboxTemplateClaimCleanupFinalizer}, Annotations: map[string]string{templatecreation.AllocationAnnotation: templateTestID}}}
	v := &api.SandboxTemplate{ObjectMeta: metav1.ObjectMeta{Namespace: c.Namespace, Name: templateTestID, UID: "version-uid", Generation: 1, OwnerReferences: []metav1.OwnerReference{{APIVersion: api.GroupVersion.String(), Kind: "SandboxTemplateClaim", Name: c.Name, UID: c.UID, Controller: ptr.To(true)}}}, Status: api.SandboxTemplateStatus{ObservedGeneration: 1, Phase: api.SandboxTemplatePhaseReady, Conditions: []metav1.Condition{{Type: templatecreation.DefinitionCondition, Status: metav1.ConditionTrue, ObservedGeneration: 1}}}}
	return c, v
}
func templateTestClient(t *testing.T, objects ...client.Object) client.WithWatch {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := api.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).WithStatusSubresource(&api.SandboxTemplate{}).
		WithIndex(&api.SandboxTemplate{}, templateindex.ClaimOwner, templateindex.ClaimOwnerUID).
		WithIndex(&api.SandboxSet{}, templateindex.TemplateRef, templateindex.TemplateRefName).Build()
}
func TestTemplateStoreBoundGetAndNamespace(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*api.SandboxTemplate)
		scope  string
		want   error
	}{
		{name: "cache miss confirms persisted version"},
		{name: "wrong claim UID is missing", mutate: func(v *api.SandboxTemplate) { v.OwnerReferences[0].UID = "old-claim" }, want: infra.ErrTemplateMissing},
		{name: "unsupported namespace fails closed", scope: "another-team", want: infra.ErrTemplateScope},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c, v := templateTestFixture()
			if test.mutate != nil {
				test.mutate(v)
			}
			live := templateTestClient(t, c, v)
			lagged := templateTestClient(t, c)
			store := &templateStore{cached: lagged, writer: live, live: live, namespace: test.scope}
			claim, err := store.GetClaim(t.Context(), c.Namespace, c.Name)
			if test.scope != "" {
				if !errors.Is(err, test.want) {
					t.Fatalf("error=%v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			got, err := store.GetVersion(t.Context(), claim.Identity, v.Name)
			if !errors.Is(err, test.want) {
				t.Fatalf("error=%v want=%v", err, test.want)
			}
			if err == nil && (got.Phase != "Ready" || got.ID != v.Name) {
				t.Fatalf("observation=%+v", got)
			}
			list, err := store.ListVersions(t.Context(), claim.Identity)
			if err != nil || len(list) != 0 {
				t.Fatalf("default view must remain eventual: %v %v", list, err)
			}
		})
	}
}
func TestTemplateStoreExternalReferences(t *testing.T) {
	c, v := templateTestFixture()
	for _, test := range []struct {
		name      string
		companion bool
		kind      *string
		want      bool
	}{
		{name: "unlabeled external pool blocks", want: true}, {name: "actual companion does not block", companion: true},
		{name: "typed lookup ignores stale kind hints", kind: ptr.To("PodTemplate"), want: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			p := &api.SandboxSet{ObjectMeta: metav1.ObjectMeta{Namespace: c.Namespace, Name: "external"}, Spec: api.SandboxSetSpec{EmbeddedSandboxTemplate: api.EmbeddedSandboxTemplate{TemplateRef: &api.SandboxTemplateRef{Name: v.Name, Kind: test.kind}}}}
			if test.companion {
				p.Name = v.Name
				p.OwnerReferences = []metav1.OwnerReference{{APIVersion: api.GroupVersion.String(), Kind: "SandboxTemplate", Name: v.Name, UID: v.UID, Controller: ptr.To(true)}}
			}
			live := templateTestClient(t, c, v)
			lagged := templateTestClient(t, c, p)
			s := &templateStore{cached: lagged, writer: live, live: live}
			got, err := s.HasExternalReferences(t.Context(), infra.TemplateClaimIdentity{Namespace: c.Namespace, Name: c.Name, UID: string(c.UID)})
			if err != nil || got != test.want {
				t.Fatalf("external=%v error=%v", got, err)
			}
		})
	}
}
func TestTemplateStorePersistedFinalizersAndPartialFailure(t *testing.T) {
	for _, failVersion := range []bool{false, true} {
		t.Run(fmt.Sprintf("versionFailure=%v", failVersion), func(t *testing.T) {
			base := templateTestClient(t)
			writer := interceptor.NewClient(base, interceptor.Funcs{Create: func(ctx context.Context, c client.WithWatch, o client.Object, options ...client.CreateOption) error {
				if _, ok := o.(*api.SandboxTemplate); ok && failVersion {
					return apierrors.NewServiceUnavailable("injected persistence failure")
				}
				o.SetUID(types.UID(o.GetName() + "-uid"))
				o.SetCreationTimestamp(metav1.Now())
				return c.Create(ctx, o, options...)
			}})
			s := &templateStore{cached: base, writer: writer, live: base}
			claim, err := s.CreateClaim(t.Context(), "team-a", "python-agent", templateTestID)
			if err != nil {
				t.Fatal(err)
			}
			d, err := templateconfig.NormalizeDefinition(templateconfig.Definition{Pod: corev1.PodSpec{Containers: []corev1.Container{{Name: "main", Image: "python:3.12"}}}})
			if err != nil {
				t.Fatal(err)
			}
			err = s.CreateVersion(t.Context(), claim, templateTestID, d)
			if failVersion && !errors.Is(err, infra.ErrTemplateViewUnavailable) || !failVersion && err != nil {
				t.Fatalf("error=%v", err)
			}
			persisted := &api.SandboxTemplateClaim{}
			if err := base.Get(t.Context(), client.ObjectKey{Namespace: claim.Namespace, Name: claim.Name}, persisted); err != nil {
				t.Fatal(err)
			}
			if len(persisted.Finalizers) != 1 || persisted.Annotations[templatecreation.AllocationAnnotation] != templateTestID {
				t.Fatalf("claim=%+v", persisted)
			}
			if failVersion {
				if persisted.Annotations[templatecreation.CreationCompleteAnnotation] != "" {
					t.Fatal("ambiguous write cannot publish positive completion")
				}
				return
			}
			version := &api.SandboxTemplate{}
			if err := base.Get(t.Context(), client.ObjectKey{Namespace: claim.Namespace, Name: templateTestID}, version); err != nil {
				t.Fatal(err)
			}
			if len(version.Finalizers) != 0 || version.OwnerReferences[0].UID != persisted.UID || !ptr.Deref(version.OwnerReferences[0].BlockOwnerDeletion, false) || version.Spec.BuildOptions.WarmPool.Size == nil {
				t.Fatalf("version=%+v", version)
			}
		})
	}
}
func TestTemplateStorePinnedDeletionDoesNotFollowReplacement(t *testing.T) {
	c, _ := templateTestFixture()
	newClaim := c.DeepCopy()
	newClaim.UID = "replacement-uid"
	live := templateTestClient(t, newClaim)
	// The existing fake does not enforce UID DeleteOptions. Model the
	// API-server precondition explicitly rather than weakening production safety.
	writer := interceptor.NewClient(live, interceptor.Funcs{Delete: func(ctx context.Context, c client.WithWatch, o client.Object, options ...client.DeleteOption) error {
		current := &api.SandboxTemplateClaim{}
		if err := c.Get(ctx, client.ObjectKeyFromObject(o), current); err != nil {
			return err
		}
		opts := (&client.DeleteOptions{}).ApplyOptions(options)
		if opts.Preconditions == nil || opts.Preconditions.UID == nil {
			t.Fatal("missing UID delete precondition")
		}
		if *opts.Preconditions.UID != current.UID {
			return apierrors.NewConflict(api.GroupVersion.WithResource("sandboxtemplateclaims").GroupResource(), current.Name, errors.New("UID precondition"))
		}
		return c.Delete(ctx, o, options...)
	}})
	s := &templateStore{cached: live, writer: writer, live: live}
	old := infra.TemplateClaimIdentity{Namespace: c.Namespace, Name: c.Name, UID: string(c.UID)}
	if err := s.BeginDelete(t.Context(), old); !errors.Is(err, infra.ErrTemplateIdentityChanged) {
		t.Fatalf("error=%v", err)
	}
	done, err := s.Deleted(t.Context(), old)
	if err != nil || !done {
		t.Fatalf("done=%v err=%v", done, err)
	}
	current := &api.SandboxTemplateClaim{}
	if err := live.Get(t.Context(), client.ObjectKeyFromObject(newClaim), current); err != nil || current.DeletionTimestamp != nil {
		t.Fatalf("replacement mutated: %+v %v", current, err)
	}
}
func TestTemplateStoreReadErrorsNeverBecomeAbsence(t *testing.T) {
	base := templateTestClient(t)
	broken := interceptor.NewClient(base, interceptor.Funcs{Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
		return apierrors.NewServiceUnavailable("unavailable")
	}})
	s := &templateStore{cached: base, writer: base, live: broken}
	_, err := s.GetClaim(t.Context(), "team-a", "python-agent")
	if !errors.Is(err, infra.ErrTemplateViewUnavailable) {
		t.Fatalf("error=%v", err)
	}
	_, err = s.Deleted(t.Context(), infra.TemplateClaimIdentity{Namespace: "team-a", Name: "python-agent", UID: "uid"})
	if !errors.Is(err, infra.ErrTemplateViewUnavailable) {
		t.Fatalf("error=%v", err)
	}
}

func TestTemplateVersionObservationCurrentDefinition(t *testing.T) {
	for _, state := range []string{"ready", "empty phase", "generation stale", "condition stale", "ready disagrees", "failed", "failed wrong reason", "building"} {
		t.Run(state, func(t *testing.T) {
			_, v := templateTestFixture()
			want, message := "Pending", ""
			switch state {
			case "ready":
				want = "Ready"
			case "empty phase":
				v.Status.Phase = ""
			case "generation stale":
				v.Generation++
			case "condition stale":
				v.Status.Conditions[0].ObservedGeneration++
			case "ready disagrees":
				v.Status.Conditions[0].Status = metav1.ConditionFalse
			case "failed", "failed wrong reason":
				v.Status.Phase = api.SandboxTemplatePhaseFailed
				v.Status.Conditions[0].Status = metav1.ConditionFalse
				v.Status.Conditions[0].Reason = templatecreation.DefinitionInvalidReason
				v.Status.Conditions[0].Message = "Invalid definition."
				if state == "failed" {
					want, message = "Failed", v.Status.Conditions[0].Message
				} else {
					v.Status.Conditions[0].Reason = "TransientFailure"
				}
			case "building":
				v.Status.Phase = api.SandboxTemplatePhaseBuilding
				want = "Building"
			}
			got := observeTemplateVersion(v)
			if got.Phase != want || got.Message != message {
				t.Fatalf("observation=%+v want=%s message=%q", got, want, message)
			}
		})
	}
}

func TestTemplateStoreDeletionConfirmsOnlyNegativeCacheReads(t *testing.T) {
	for _, state := range []string{"current", "cache missing", "cache old UID", "absent", "replacement"} {
		t.Run(state, func(t *testing.T) {
			c, _ := templateTestFixture()
			cachedObjects, liveObjects := []client.Object{c}, []client.Object{c}
			switch state {
			case "cache missing":
				cachedObjects = nil
			case "cache old UID":
				old := c.DeepCopy()
				old.UID = "old-uid"
				cachedObjects = []client.Object{old}
			case "absent":
				cachedObjects, liveObjects = nil, nil
			case "replacement":
				newClaim := c.DeepCopy()
				newClaim.UID = "replacement"
				cachedObjects, liveObjects = []client.Object{newClaim}, []client.Object{newClaim}
			}
			cached, live := templateTestClient(t, cachedObjects...), templateTestClient(t, liveObjects...)
			reads := 0
			reader := interceptor.NewClient(live, interceptor.Funcs{Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				reads++
				return cl.Get(ctx, key, obj, opts...)
			}})
			s := &templateStore{cached: cached, live: reader}
			done, err := s.Deleted(t.Context(), infra.TemplateClaimIdentity{Namespace: c.Namespace, Name: c.Name, UID: string(c.UID)})
			wantReads := 1
			if state == "current" {
				wantReads = 0
			}
			if err != nil || done != (state == "absent" || state == "replacement") || reads != wantReads {
				t.Fatalf("done=%v reads=%d err=%v", done, reads, err)
			}
		})
	}
}

type deadlineCaptureTransport struct{ request *http.Request }

func (t *deadlineCaptureTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	t.request = r
	return &http.Response{StatusCode: 201, Header: http.Header{}, Body: http.NoBody, Request: r}, nil
}
func TestTemplateCreateTransportDeadline(t *testing.T) {
	capture := &deadlineCaptureTransport{}
	transport := templateRequestDeadlineTransport{next: capture}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://cluster/apis/agents.kruise.io/v1alpha1/namespaces/team-a/sandboxtemplates", http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := transport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatal(err)
	}
	timeout, err := time.ParseDuration(capture.request.URL.Query().Get("timeout"))
	if err != nil || timeout <= 0 || timeout > time.Second {
		t.Fatalf("timeout=%v err=%v", timeout, err)
	}
	if req.URL.RawQuery != "" {
		t.Fatal("transport mutated caller request")
	}
}

// Only Start/WaitForCacheSync are needed to exercise cache lifecycle races.
// The embedded cache interface is never used for object reads in this test.
type templateLifecycleCache struct {
	ctrlcache.Cache
}

func (c *templateLifecycleCache) Start(ctx context.Context) error           { <-ctx.Done(); return nil }
func (c *templateLifecycleCache) WaitForCacheSync(ctx context.Context) bool { return ctx.Err() == nil }
func TestTemplateViewRunStopHealth(t *testing.T) {
	for i := 0; i < 10; i++ {
		v := &templateView{cache: &templateLifecycleCache{}}
		if err := v.Run(t.Context()); err != nil {
			t.Fatal(err)
		}
		if !v.Healthy() {
			t.Fatal("synced live cache should be healthy")
		}
		v.Stop(t.Context())
		if v.Healthy() {
			t.Fatal("stopped cache cannot serve")
		}
		if err := v.Run(t.Context()); err == nil {
			t.Fatal("stopped view must not restart")
		}
	}
}
func TestTemplateViewStopBeforeRun(t *testing.T) {
	v := &templateView{cache: &templateLifecycleCache{}}
	v.Stop(t.Context())
	if err := v.Run(t.Context()); err == nil {
		t.Fatal("stop-before-start must reject late startup")
	}
}

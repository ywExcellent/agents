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

package sandbox_manager

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	managererrors "github.com/openkruise/agents/pkg/sandbox-manager/errors"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
	"github.com/openkruise/agents/pkg/sandbox-manager/templatecr"
)

type templateStoreFuncs struct {
	get      func(context.Context, string, string) (*agentsv1alpha1.SandboxTemplate, error)
	getFresh func(context.Context, string, string) (*agentsv1alpha1.SandboxTemplate, error)
	create   func(context.Context, *agentsv1alpha1.SandboxTemplate) error
	update   func(context.Context, *agentsv1alpha1.SandboxTemplate) error
	delete   func(context.Context, string, string, types.UID, string) error
}

func (s *templateStoreFuncs) Get(ctx context.Context, namespace, name string) (*agentsv1alpha1.SandboxTemplate, error) {
	return s.get(ctx, namespace, name)
}

func (s *templateStoreFuncs) GetFresh(ctx context.Context, namespace, name string) (*agentsv1alpha1.SandboxTemplate, error) {
	return s.getFresh(ctx, namespace, name)
}

func (s *templateStoreFuncs) Create(ctx context.Context, template *agentsv1alpha1.SandboxTemplate) error {
	return s.create(ctx, template)
}

func (s *templateStoreFuncs) Update(ctx context.Context, template *agentsv1alpha1.SandboxTemplate) error {
	return s.update(ctx, template)
}

func (s *templateStoreFuncs) Delete(ctx context.Context, namespace, name string, uid types.UID, resourceVersion string) error {
	return s.delete(ctx, namespace, name, uid, resourceVersion)
}

func templateNotFound(name string) error {
	return apierrors.NewNotFound(schema.GroupResource{Group: agentsv1alpha1.GroupVersion.Group, Resource: "sandboxtemplates"}, name)
}

// stubRead returns a store read that always yields the given template and error.
func stubRead(template *agentsv1alpha1.SandboxTemplate, err error) func(context.Context, string, string) (*agentsv1alpha1.SandboxTemplate, error) {
	return func(context.Context, string, string) (*agentsv1alpha1.SandboxTemplate, error) {
		return template.DeepCopy(), err
	}
}

// stubStore serves cached and fresh reads from one fixed result; layer
// withCreate, withUpdate, or withDelete to control writes.
func stubStore(template *agentsv1alpha1.SandboxTemplate, err error) *templateStoreFuncs {
	return &templateStoreFuncs{get: stubRead(template, err), getFresh: stubRead(template, err)}
}

func (s *templateStoreFuncs) withCreate(err error) *templateStoreFuncs {
	s.create = func(context.Context, *agentsv1alpha1.SandboxTemplate) error { return err }
	return s
}

func (s *templateStoreFuncs) withUpdate(err error) *templateStoreFuncs {
	s.update = func(context.Context, *agentsv1alpha1.SandboxTemplate) error { return err }
	return s
}

func (s *templateStoreFuncs) withDelete(err error) *templateStoreFuncs {
	s.delete = func(context.Context, string, string, types.UID, string) error { return err }
	return s
}

type fixedTemplateNameConflictChecker struct {
	conflict infra.LogicalTemplateNameConflict
	err      error
}

func (c fixedTemplateNameConflictChecker) CheckLogicalTemplateNameConflict(context.Context, infra.CheckLogicalTemplateNameConflictOptions) (infra.LogicalTemplateNameConflict, error) {
	return c.conflict, c.err
}

// flakyTemplateNameConflictChecker succeeds on the first check and fails on
// every later one, so a test can pass the preflight name check and still make
// the retry-time check fail.
type flakyTemplateNameConflictChecker struct {
	calls int
	err   error
}

func (c *flakyTemplateNameConflictChecker) CheckLogicalTemplateNameConflict(context.Context, infra.CheckLogicalTemplateNameConflictOptions) (infra.LogicalTemplateNameConflict, error) {
	c.calls++
	if c.calls > 1 {
		return infra.LogicalTemplateNameConflictNone, c.err
	}
	return infra.LogicalTemplateNameConflictNone, nil
}

type testTemplateNameConflictChecker struct {
	client ctrlclient.Client
}

func (c testTemplateNameConflictChecker) CheckLogicalTemplateNameConflict(ctx context.Context, opts infra.CheckLogicalTemplateNameConflictOptions) (infra.LogicalTemplateNameConflict, error) {
	key := ctrlclient.ObjectKey{Namespace: opts.Namespace, Name: opts.Name}
	for _, candidate := range []struct {
		conflict infra.LogicalTemplateNameConflict
		object   ctrlclient.Object
	}{
		{conflict: infra.LogicalTemplateNameConflictSandboxSet, object: &agentsv1alpha1.SandboxSet{}},
		{conflict: infra.LogicalTemplateNameConflictCheckpoint, object: &agentsv1alpha1.Checkpoint{}},
	} {
		err := c.client.Get(ctx, key, candidate.object)
		if err == nil {
			return candidate.conflict, nil
		}
		if !apierrors.IsNotFound(err) {
			return infra.LogicalTemplateNameConflictNone, err
		}
	}
	return infra.LogicalTemplateNameConflictNone, nil
}

func newTemplateManager(t *testing.T, objects ...ctrlclient.Object) (*SandboxManager, ctrlclient.Client) {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, agentsv1alpha1.AddToScheme(scheme))
	uid := 0
	client := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&agentsv1alpha1.SandboxTemplate{}).
		WithObjects(objects...).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, c ctrlclient.WithWatch, obj ctrlclient.Object, opts ...ctrlclient.CreateOption) error {
				if template, ok := obj.(*agentsv1alpha1.SandboxTemplate); ok {
					uid++
					template.UID = types.UID(fmt.Sprintf("00000000-0000-0000-0000-%012d", uid))
					template.CreationTimestamp = metav1.Now()
				}
				return c.Create(ctx, obj, opts...)
			},
		}).
		Build()
	return &SandboxManager{
		templateStore:               templatecr.NewStore(client, func() ctrlclient.Reader { return client }),
		templateNameConflictChecker: testTemplateNameConflictChecker{client: client},
	}, client
}

func validLogicalTemplateSpec(image string) agentsv1alpha1.SandboxTemplateSpec {
	return agentsv1alpha1.SandboxTemplateSpec{
		Backend: agentsv1alpha1.SandboxTemplateBackendKubernetes,
		Template: &corev1.PodTemplateSpec{Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "runtime", Image: image}},
		}},
	}
}

// submitEnvelopeSpec matches the envelope of a 1-cpu, 1024MiB reservation.
func submitEnvelopeSpec() agentsv1alpha1.SandboxTemplateSpec {
	cpu := resource.MustParse("1")
	memory := resource.MustParse("1024Mi")
	spec := validLogicalTemplateSpec("image")
	spec.Template.Spec.Containers[0].Resources = corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceCPU: cpu, corev1.ResourceMemory: memory},
		Limits:   corev1.ResourceList{corev1.ResourceCPU: cpu, corev1.ResourceMemory: memory},
	}
	return spec
}

// reservationDraft builds a reserved draft carrying the controller's optional
// failure verdict, shared by the submit and get availability tests.
func reservationDraft(reserve ReserveTemplateOptions, createdAt time.Time, phase agentsv1alpha1.SandboxTemplateBuildPhase, reason string) *agentsv1alpha1.SandboxTemplate {
	draft := reservedTemplate(reserve, agentsv1alpha1.SandboxTemplateBackendKubernetes)
	draft.UID = "reserved-uid"
	draft.CreationTimestamp = metav1.NewTime(createdAt)
	draft.Status.Phase = phase
	if reason != "" {
		draft.Status.Conditions = []metav1.Condition{{
			Type:   agentsv1alpha1.SandboxTemplateBuildConditionReady,
			Status: metav1.ConditionFalse,
			Reason: reason,
		}}
	}
	return draft
}

func TestCreateTemplate(t *testing.T) {
	manager, client := newTemplateManager(t)
	spec := validLogicalTemplateSpec("python:3.12")
	spec.PersistentContents = []string{agentsv1alpha1.PersistentContentFilesystem}
	spec.BuildOptions = &agentsv1alpha1.SandboxTemplateBuildOptions{
		Steps: []agentsv1alpha1.TemplateBuildCommand{{Command: "pip install numpy"}},
	}
	opts := CreateTemplateOptions{Namespace: "team-a", Name: "python-agent", Spec: spec}

	created, isNew, err := manager.CreateTemplate(t.Context(), opts)
	require.NoError(t, err)
	assert.True(t, isNew)
	assert.NotEmpty(t, created.BuildID)
	assert.Equal(t, agentsv1alpha1.SandboxTemplatePhasePending, created.Status.Phase)

	_, _, err = manager.CreateTemplate(t.Context(), CreateTemplateOptions{Namespace: "team-a", Name: "plain-template", Spec: validLogicalTemplateSpec("image")})
	require.NoError(t, err)
	plain := &agentsv1alpha1.SandboxTemplate{}
	require.NoError(t, client.Get(t.Context(), types.NamespacedName{Namespace: "team-a", Name: "plain-template"}, plain))
	assert.Nil(t, plain.Spec.BuildOptions)

	stored := &agentsv1alpha1.SandboxTemplate{}
	require.NoError(t, client.Get(t.Context(), types.NamespacedName{Namespace: opts.Namespace, Name: opts.Name}, stored))
	assert.Equal(t, agentsv1alpha1.TemplateBuildStateSubmitted, stored.Annotations[agentsv1alpha1.AnnotationTemplateBuildState])
	assert.NotEmpty(t, stored.Annotations[agentsv1alpha1.AnnotationTemplateBuildInputDigest])
	require.NotNil(t, stored.Spec.BuildOptions)

	replayed, isNew, err := manager.CreateTemplate(t.Context(), opts)
	require.NoError(t, err)
	assert.False(t, isNew)
	assert.Equal(t, created.BuildID, replayed.BuildID)

	changed := opts
	changed.Spec = *opts.Spec.DeepCopy()
	changed.Spec.Template.Spec.Containers[0].Image = "python:3.13"
	_, _, err = manager.CreateTemplate(t.Context(), changed)
	require.Error(t, err)
	assert.Equal(t, managererrors.ErrorConflict, managererrors.GetErrCode(err))
}

func TestCreateTemplateReclaimsExpiredReservation(t *testing.T) {
	draft := reservedTemplate(ReserveTemplateOptions{Namespace: "team-a", Name: "template", CPUCount: 1, MemoryMB: 1024}, agentsv1alpha1.SandboxTemplateBackendKubernetes)
	draft.UID = "expired-uid"
	draft.CreationTimestamp = metav1.NewTime(time.Now().Add(-2 * agentsv1alpha1.SandboxTemplateBuildReservationTTL))
	manager, client := newTemplateManager(t, draft)

	created, isNew, err := manager.CreateTemplate(t.Context(), CreateTemplateOptions{
		Namespace: "team-a", Name: "template", Spec: validLogicalTemplateSpec("python:3.12"),
	})
	require.NoError(t, err)
	assert.True(t, isNew)
	assert.NotEqual(t, "expired-uid", created.BuildID)

	stored := &agentsv1alpha1.SandboxTemplate{}
	require.NoError(t, client.Get(t.Context(), types.NamespacedName{Namespace: "team-a", Name: "template"}, stored))
	assert.Equal(t, agentsv1alpha1.TemplateBuildStateSubmitted, stored.Annotations[agentsv1alpha1.AnnotationTemplateBuildState])
}

func TestCreateTemplateValidationAndNameConflicts(t *testing.T) {
	substrateSpec := validLogicalTemplateSpec("image")
	substrateSpec.Backend = agentsv1alpha1.SandboxTemplateBackendSubstrate
	tests := []struct {
		name     string
		objects  []ctrlclient.Object
		opts     CreateTemplateOptions
		wantCode managererrors.ErrorCode
	}{
		{name: "reserved name", opts: CreateTemplateOptions{Namespace: "team-a", Name: "tags", Spec: validLogicalTemplateSpec("image")}, wantCode: managererrors.ErrorBadRequest},
		{name: "unsupported backend", opts: CreateTemplateOptions{Namespace: "team-a", Name: "template", Spec: substrateSpec}, wantCode: managererrors.ErrorNotImplemented},
		{name: "SandboxSet name conflict", objects: []ctrlclient.Object{&agentsv1alpha1.SandboxSet{ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "template"}}}, opts: CreateTemplateOptions{Namespace: "team-a", Name: "template", Spec: validLogicalTemplateSpec("image")}, wantCode: managererrors.ErrorConflict},
		{name: "Checkpoint name conflict", objects: []ctrlclient.Object{&agentsv1alpha1.Checkpoint{ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "template"}}}, opts: CreateTemplateOptions{Namespace: "team-a", Name: "template", Spec: validLogicalTemplateSpec("image")}, wantCode: managererrors.ErrorConflict},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			manager, _ := newTemplateManager(t, tt.objects...)
			_, _, err := manager.CreateTemplate(t.Context(), tt.opts)
			require.Error(t, err)
			assert.Equal(t, tt.wantCode, managererrors.GetErrCode(err))
		})
	}
}

func TestReserveAndSubmitTemplate(t *testing.T) {
	manager, client := newTemplateManager(t)
	reserve := ReserveTemplateOptions{Namespace: "team-a", Name: "python-agent", CPUCount: 2, MemoryMB: 2048}

	reserved, err := manager.ReserveTemplate(t.Context(), reserve)
	require.NoError(t, err)
	assert.NotEmpty(t, reserved.BuildID)
	assert.Equal(t, agentsv1alpha1.SandboxTemplatePhasePending, reserved.Status.Phase)

	replayed, err := manager.ReserveTemplate(t.Context(), reserve)
	require.NoError(t, err)
	assert.Equal(t, reserved.BuildID, replayed.BuildID)

	changedReservation := reserve
	changedReservation.MemoryMB = 1024
	_, err = manager.ReserveTemplate(t.Context(), changedReservation)
	require.Error(t, err)
	assert.Equal(t, managererrors.ErrorConflict, managererrors.GetErrCode(err))

	cpu := resource.MustParse("2")
	memory := resource.MustParse("2048Mi")
	resources := corev1.ResourceList{corev1.ResourceCPU: cpu, corev1.ResourceMemory: memory}
	spec := validLogicalTemplateSpec("python:3.12")
	spec.Template.Spec.Containers[0].Resources = corev1.ResourceRequirements{
		Requests: resources.DeepCopy(), Limits: resources.DeepCopy(),
	}
	spec.PersistentContents = []string{agentsv1alpha1.PersistentContentFilesystem}
	spec.BuildOptions = &agentsv1alpha1.SandboxTemplateBuildOptions{Steps: []agentsv1alpha1.TemplateBuildCommand{{Command: "pip install numpy"}}}
	submit := SubmitTemplateOptions{Namespace: reserve.Namespace, Name: reserve.Name, BuildID: reserved.BuildID, Spec: spec}

	mismatchedBackend := submit
	mismatchedBackend.Spec = *submit.Spec.DeepCopy()
	mismatchedBackend.Spec.Backend = agentsv1alpha1.SandboxTemplateBackendSubstrate
	_, err = manager.SubmitTemplate(t.Context(), mismatchedBackend)
	require.Error(t, err)
	assert.Equal(t, managererrors.ErrorConflict, managererrors.GetErrCode(err))

	submitted, err := manager.SubmitTemplate(t.Context(), submit)
	require.NoError(t, err)
	assert.Equal(t, reserved.BuildID, submitted.BuildID)
	stored := &agentsv1alpha1.SandboxTemplate{}
	require.NoError(t, client.Get(t.Context(), types.NamespacedName{Namespace: reserve.Namespace, Name: reserve.Name}, stored))
	assert.Equal(t, agentsv1alpha1.TemplateBuildStateSubmitted, stored.Annotations[agentsv1alpha1.AnnotationTemplateBuildState])
	assert.NotEmpty(t, stored.Annotations[agentsv1alpha1.AnnotationTemplateBuildInputDigest])

	_, err = manager.SubmitTemplate(t.Context(), submit)
	require.NoError(t, err)

	changedSubmit := submit
	changedSubmit.Spec = *submit.Spec.DeepCopy()
	changedSubmit.Spec.Template.Spec.Containers[0].Image = "python:3.13"
	_, err = manager.SubmitTemplate(t.Context(), changedSubmit)
	require.Error(t, err)
	assert.Equal(t, managererrors.ErrorConflict, managererrors.GetErrCode(err))
}

func TestSubmitTemplateRejectsUnavailableReservation(t *testing.T) {
	reserve := ReserveTemplateOptions{Namespace: "team-a", Name: "template", CPUCount: 1, MemoryMB: 1024}
	tests := []struct {
		name      string
		createdAt time.Time
		phase     agentsv1alpha1.SandboxTemplateBuildPhase
		reason    string
		wantCode  managererrors.ErrorCode
	}{
		{name: "ttl elapsed before controller status update", createdAt: time.Now().Add(-2 * agentsv1alpha1.SandboxTemplateBuildReservationTTL), wantCode: managererrors.ErrorNotFound},
		{name: "controller already marked reservation expired", createdAt: time.Now(), phase: agentsv1alpha1.SandboxTemplatePhaseFailed, reason: agentsv1alpha1.SandboxTemplateBuildReasonReservationExpired, wantCode: managererrors.ErrorNotFound},
		{name: "non-expiration failure remains terminal", createdAt: time.Now(), phase: agentsv1alpha1.SandboxTemplatePhaseFailed, reason: "ValidationFailed", wantCode: managererrors.ErrorConflict},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			manager, client := newTemplateManager(t, reservationDraft(reserve, tt.createdAt, tt.phase, tt.reason))
			_, err := manager.SubmitTemplate(t.Context(), SubmitTemplateOptions{
				Namespace: reserve.Namespace, Name: reserve.Name, BuildID: "reserved-uid", Spec: submitEnvelopeSpec(),
			})
			require.Error(t, err)
			assert.Equal(t, tt.wantCode, managererrors.GetErrCode(err))

			stored := &agentsv1alpha1.SandboxTemplate{}
			require.NoError(t, client.Get(t.Context(), types.NamespacedName{Namespace: reserve.Namespace, Name: reserve.Name}, stored))
			assert.Equal(t, agentsv1alpha1.TemplateBuildStateReserved, stored.Annotations[agentsv1alpha1.AnnotationTemplateBuildState])
		})
	}
}

func TestSubmitTemplateReplaysSubmittedBuildAfterReservationTTL(t *testing.T) {
	spec := validLogicalTemplateSpec("python:3.12")
	digest, err := templateInputDigest(spec)
	require.NoError(t, err)
	template := &agentsv1alpha1.SandboxTemplate{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:         "team-a",
			Name:              "python-agent",
			UID:               "submitted-uid",
			CreationTimestamp: metav1.NewTime(time.Now().Add(-2 * agentsv1alpha1.SandboxTemplateBuildReservationTTL)),
			Annotations:       submittedTemplateAnnotations(digest),
		},
		Spec: spec,
	}
	manager, _ := newTemplateManager(t, template)

	details, err := manager.SubmitTemplate(t.Context(), SubmitTemplateOptions{
		Namespace: template.Namespace, Name: template.Name, BuildID: string(template.UID), Spec: spec,
	})
	require.NoError(t, err)
	assert.Equal(t, string(template.UID), details.BuildID)
}

func TestGetTemplateHidesExpiredReservation(t *testing.T) {
	reserve := ReserveTemplateOptions{Namespace: "team-a", Name: "template", CPUCount: 1, MemoryMB: 1024}
	tests := []struct {
		name      string
		createdAt time.Time
		phase     agentsv1alpha1.SandboxTemplateBuildPhase
		reason    string
		wantFound bool
	}{
		{name: "ttl elapsed", createdAt: time.Now().Add(-2 * agentsv1alpha1.SandboxTemplateBuildReservationTTL), wantFound: false},
		{name: "controller marked reservation expired", createdAt: time.Now(), phase: agentsv1alpha1.SandboxTemplatePhaseFailed, reason: agentsv1alpha1.SandboxTemplateBuildReasonReservationExpired, wantFound: false},
		{name: "non-expiration failure remains visible", createdAt: time.Now(), phase: agentsv1alpha1.SandboxTemplatePhaseFailed, reason: "ValidationFailed", wantFound: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			manager, _ := newTemplateManager(t, reservationDraft(reserve, tt.createdAt, tt.phase, tt.reason))

			_, err := manager.GetTemplate(t.Context(), reserve.Namespace, reserve.Name)
			if !tt.wantFound {
				require.Error(t, err)
				assert.Equal(t, managererrors.ErrorNotFound, managererrors.GetErrCode(err))
				return
			}
			require.NoError(t, err)
		})
	}
}

func TestGetTemplateRejectsNameAmbiguityAndSubordinates(t *testing.T) {
	logical := &agentsv1alpha1.SandboxTemplate{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "team-a", Name: "template", UID: "template-uid",
			Annotations: map[string]string{agentsv1alpha1.AnnotationTemplateBuildState: agentsv1alpha1.TemplateBuildStateSubmitted},
		},
		Spec: validLogicalTemplateSpec("image"),
	}
	owned := true
	tests := []struct {
		name        string
		seedLogical bool
		objects     []ctrlclient.Object
		wantCode    managererrors.ErrorCode
	}{
		{
			name: "subordinate template",
			objects: []ctrlclient.Object{&agentsv1alpha1.SandboxTemplate{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: "team-a", Name: "template", UID: "revision-uid",
					OwnerReferences: []metav1.OwnerReference{{Controller: &owned, UID: "owner"}},
				},
				Spec: validLogicalTemplateSpec("image"),
			}},
			wantCode: managererrors.ErrorNotFound,
		},
		{name: "unrelated SandboxSet conflicts", seedLogical: true, objects: []ctrlclient.Object{&agentsv1alpha1.SandboxSet{ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "template"}}}, wantCode: managererrors.ErrorConflict},
		{name: "same named Checkpoint conflicts", seedLogical: true, objects: []ctrlclient.Object{&agentsv1alpha1.Checkpoint{ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "template"}}}, wantCode: managererrors.ErrorConflict},
		{
			// Phase 1 has no companion identity contract, so ownership alone does
			// not exempt a same-named SandboxSet from ambiguity checks.
			name:        "owned SandboxSet conflicts before companion support lands",
			seedLogical: true,
			objects: []ctrlclient.Object{&agentsv1alpha1.SandboxSet{ObjectMeta: metav1.ObjectMeta{
				Namespace:       "team-a",
				Name:            "template",
				OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(logical, agentsv1alpha1.SandboxTemplateControllerKind)},
			}}},
			wantCode: managererrors.ErrorConflict,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			objects := tt.objects
			if tt.seedLogical {
				objects = append([]ctrlclient.Object{logical.DeepCopy()}, tt.objects...)
			}
			manager, _ := newTemplateManager(t, objects...)
			_, err := manager.GetTemplate(t.Context(), "team-a", "template")
			require.Error(t, err)
			assert.Equal(t, tt.wantCode, managererrors.GetErrCode(err))
		})
	}
}

func TestReserveTemplateReclaimsExpiredDraft(t *testing.T) {
	tests := []struct {
		name     string
		memoryMB int32
	}{
		{name: "same reservation parameters", memoryMB: 1024},
		{name: "different reservation parameters", memoryMB: 2048},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			draft := reservedTemplate(ReserveTemplateOptions{Namespace: "team-a", Name: "template", CPUCount: 1, MemoryMB: 1024}, agentsv1alpha1.SandboxTemplateBackendKubernetes)
			draft.UID = "expired-uid"
			draft.ResourceVersion = "7"
			draft.CreationTimestamp = metav1.NewTime(time.Now().Add(-2 * agentsv1alpha1.SandboxTemplateBuildReservationTTL))
			manager, client := newTemplateManager(t, draft)
			deleteUsedUID, deleteUsedRV := false, false
			intercepted := interceptor.NewClient(client.(ctrlclient.WithWatch), interceptor.Funcs{
				Delete: func(ctx context.Context, c ctrlclient.WithWatch, obj ctrlclient.Object, opts ...ctrlclient.DeleteOption) error {
					deleteOptions := (&ctrlclient.DeleteOptions{}).ApplyOptions(opts)
					deleteUsedUID = deleteOptions.Preconditions != nil && deleteOptions.Preconditions.UID != nil && *deleteOptions.Preconditions.UID == draft.UID
					deleteUsedRV = deleteOptions.Preconditions != nil && deleteOptions.Preconditions.ResourceVersion != nil && *deleteOptions.Preconditions.ResourceVersion == draft.ResourceVersion
					return c.Delete(ctx, obj, opts...)
				},
			})
			manager.templateStore = templatecr.NewStore(intercepted, func() ctrlclient.Reader { return intercepted })

			created, err := manager.ReserveTemplate(t.Context(), ReserveTemplateOptions{
				Namespace: "team-a", Name: "template", CPUCount: 1, MemoryMB: tt.memoryMB,
			})
			require.NoError(t, err)
			assert.True(t, deleteUsedUID)
			assert.True(t, deleteUsedRV)
			assert.NotEqual(t, "expired-uid", created.BuildID)
		})
	}
}

func TestTemplateInputDigestIsCanonical(t *testing.T) {
	left := validLogicalTemplateSpec("image")
	left.BuildOptions = &agentsv1alpha1.SandboxTemplateBuildOptions{Steps: []agentsv1alpha1.TemplateBuildCommand{{
		Command: "prepare", Env: map[string]string{"B": "2", "A": "1"},
	}}}
	right := *left.DeepCopy()
	right.BuildOptions.Steps[0].Env = map[string]string{"A": "1", "B": "2"}

	leftDigest, err := templateInputDigest(left)
	require.NoError(t, err)
	rightDigest, err := templateInputDigest(right)
	require.NoError(t, err)
	assert.Equal(t, leftDigest, rightDigest)

	right.BuildOptions.Steps[0].Command = "prepare --changed"
	differentInput, err := templateInputDigest(right)
	require.NoError(t, err)
	assert.NotEqual(t, leftDigest, differentInput)
}

func TestCreateTemplateRecoversFromStaleCacheAfterAlreadyExists(t *testing.T) {
	opts := CreateTemplateOptions{Namespace: "team-a", Name: "template", Spec: validLogicalTemplateSpec("image")}
	digest, err := templateInputDigest(opts.Spec)
	require.NoError(t, err)
	existing := &agentsv1alpha1.SandboxTemplate{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:   opts.Namespace,
			Name:        opts.Name,
			UID:         "existing-uid",
			Annotations: submittedTemplateAnnotations(digest),
		},
		Spec: opts.Spec,
	}
	store := stubStore(nil, templateNotFound("template"))
	store.getFresh = stubRead(existing, nil)
	store.create = func(context.Context, *agentsv1alpha1.SandboxTemplate) error {
		return apierrors.NewAlreadyExists(schema.GroupResource{Group: agentsv1alpha1.GroupVersion.Group, Resource: "sandboxtemplates"}, opts.Name)
	}
	manager := &SandboxManager{templateStore: store, templateNameConflictChecker: fixedTemplateNameConflictChecker{}}

	details, created, err := manager.CreateTemplate(t.Context(), opts)
	require.NoError(t, err)
	assert.False(t, created)
	assert.Equal(t, string(existing.UID), details.BuildID)
}

func TestReserveTemplateRecoversFromStaleCacheAfterAlreadyExists(t *testing.T) {
	opts := ReserveTemplateOptions{Namespace: "team-a", Name: "template", CPUCount: 1, MemoryMB: 1024}
	existing := reservedTemplate(opts, agentsv1alpha1.SandboxTemplateBackendKubernetes)
	existing.UID = "existing-uid"
	existing.CreationTimestamp = metav1.Now()
	store := stubStore(nil, templateNotFound("template"))
	store.getFresh = stubRead(existing, nil)
	store.create = func(context.Context, *agentsv1alpha1.SandboxTemplate) error {
		return apierrors.NewAlreadyExists(schema.GroupResource{Group: agentsv1alpha1.GroupVersion.Group, Resource: "sandboxtemplates"}, opts.Name)
	}
	manager := &SandboxManager{templateStore: store, templateNameConflictChecker: fixedTemplateNameConflictChecker{}}

	details, err := manager.ReserveTemplate(t.Context(), opts)
	require.NoError(t, err)
	assert.Equal(t, string(existing.UID), details.BuildID)
}

func TestSubmitTemplateRecoversFromStaleCacheAfterConflict(t *testing.T) {
	reserve := ReserveTemplateOptions{Namespace: "team-a", Name: "template", CPUCount: 1, MemoryMB: 1024}
	stale := reservedTemplate(reserve, agentsv1alpha1.SandboxTemplateBackendKubernetes)
	stale.UID = "build-uid"
	stale.CreationTimestamp = metav1.Now()
	spec := submitEnvelopeSpec()
	digest, err := templateInputDigest(spec)
	require.NoError(t, err)
	latest := stale.DeepCopy()
	latest.Spec = *spec.DeepCopy()
	latest.Annotations = submittedTemplateAnnotations(digest)
	store := stubStore(stale, nil)
	store.getFresh = stubRead(latest, nil)
	store.update = func(context.Context, *agentsv1alpha1.SandboxTemplate) error {
		return apierrors.NewConflict(schema.GroupResource{Group: agentsv1alpha1.GroupVersion.Group, Resource: "sandboxtemplates"}, stale.Name, errors.New("stale resourceVersion"))
	}
	manager := &SandboxManager{templateStore: store, templateNameConflictChecker: fixedTemplateNameConflictChecker{}}

	details, err := manager.SubmitTemplate(t.Context(), SubmitTemplateOptions{
		Namespace: stale.Namespace, Name: stale.Name, BuildID: string(stale.UID), Spec: spec,
	})
	require.NoError(t, err)
	assert.Equal(t, string(stale.UID), details.BuildID)
}

func TestTemplateStorageErrorClassification(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want managererrors.ErrorCode
	}{
		{name: "conflict", err: apierrors.NewConflict(schema.GroupResource{Group: "agents.kruise.io", Resource: "sandboxtemplates"}, "t", errors.New("conflict")), want: managererrors.ErrorConflict},
		{name: "invalid", err: apierrors.NewInvalid(agentsv1alpha1.GroupVersion.WithKind("SandboxTemplate").GroupKind(), "t", field.ErrorList{field.Invalid(field.NewPath("spec"), nil, "invalid")}), want: managererrors.ErrorBadRequest},
		{name: "not found", err: apierrors.NewNotFound(schema.GroupResource{Group: "agents.kruise.io", Resource: "sandboxtemplates"}, "t"), want: managererrors.ErrorNotFound},
		{name: "too many requests", err: apierrors.NewTooManyRequests("busy", 1), want: managererrors.ErrorUnavailable},
		{name: "unknown", err: errors.New("boom"), want: managererrors.ErrorInternal},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, managererrors.GetErrCode(templateStorageError("operation", tt.err)))
		})
	}
}

func TestReserveTemplateRejectsInvalidInput(t *testing.T) {
	manager := &SandboxManager{}
	tests := []struct {
		name     string
		opts     ReserveTemplateOptions
		wantCode managererrors.ErrorCode
	}{
		{name: "unsupported backend", opts: ReserveTemplateOptions{Namespace: "team-a", Name: "template", Backend: agentsv1alpha1.SandboxTemplateBackendSubstrate, CPUCount: 1, MemoryMB: 1024}, wantCode: managererrors.ErrorNotImplemented},
		{name: "zero cpu", opts: ReserveTemplateOptions{Namespace: "team-a", Name: "template", CPUCount: 0, MemoryMB: 1024}, wantCode: managererrors.ErrorBadRequest},
		{name: "zero memory", opts: ReserveTemplateOptions{Namespace: "team-a", Name: "template", CPUCount: 1, MemoryMB: 0}, wantCode: managererrors.ErrorBadRequest},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := manager.ReserveTemplate(t.Context(), tt.opts)
			require.Error(t, err)
			assert.Equal(t, tt.wantCode, managererrors.GetErrCode(err))
		})
	}
}

func TestReserveTemplateRejectsStorageFailures(t *testing.T) {
	opts := ReserveTemplateOptions{Namespace: "team-a", Name: "template", CPUCount: 1, MemoryMB: 1024}
	tests := []struct {
		name     string
		store    *templateStoreFuncs
		wantCode managererrors.ErrorCode
	}{
		{name: "reservation read failure", store: stubStore(nil, errors.New("read boom")), wantCode: managererrors.ErrorInternal},
		{name: "reservation create failure", store: stubStore(nil, templateNotFound("template")).withCreate(errors.New("write boom")), wantCode: managererrors.ErrorInternal},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			manager := &SandboxManager{templateStore: tt.store, templateNameConflictChecker: fixedTemplateNameConflictChecker{}}
			_, err := manager.ReserveTemplate(t.Context(), opts)
			require.Error(t, err)
			assert.Equal(t, tt.wantCode, managererrors.GetErrCode(err))
		})
	}
}

func TestCreateTemplateRejectsStorageFailures(t *testing.T) {
	opts := CreateTemplateOptions{Namespace: "team-a", Name: "template", Spec: validLogicalTemplateSpec("image")}
	existing := reservedTemplate(ReserveTemplateOptions{Namespace: "team-a", Name: "template", CPUCount: 1, MemoryMB: 1024}, agentsv1alpha1.SandboxTemplateBackendKubernetes)
	existing.UID = "existing-uid"
	existing.CreationTimestamp = metav1.Now()
	tests := []struct {
		name    string
		store   *templateStoreFuncs
		checker infra.LogicalTemplateNameConflictChecker
	}{
		{name: "template create failure", store: stubStore(nil, templateNotFound("template")).withCreate(errors.New("write boom"))},
		{name: "name conflict check failure", store: stubStore(nil, templateNotFound("template")), checker: fixedTemplateNameConflictChecker{err: errors.New("checker boom")}},
		{name: "existing template ambiguity check failure", store: stubStore(existing, nil), checker: fixedTemplateNameConflictChecker{err: errors.New("checker boom")}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			manager := &SandboxManager{templateStore: tt.store, templateNameConflictChecker: fixedTemplateNameConflictChecker{}}
			if tt.checker != nil {
				manager.templateNameConflictChecker = tt.checker
			}
			_, _, err := manager.CreateTemplate(t.Context(), opts)
			require.Error(t, err)
			assert.Equal(t, managererrors.ErrorInternal, managererrors.GetErrCode(err))
		})
	}
}

func TestCreateTemplateRejectsExistingTemplates(t *testing.T) {
	owned := true
	existing := &agentsv1alpha1.SandboxTemplate{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "team-a", Name: "template", UID: "revision-uid",
			OwnerReferences: []metav1.OwnerReference{{Controller: &owned, UID: "owner"}},
		},
		Spec: validLogicalTemplateSpec("image"),
	}
	manager, _ := newTemplateManager(t, existing)

	_, _, err := manager.CreateTemplate(t.Context(), CreateTemplateOptions{
		Namespace: "team-a", Name: "template", Spec: validLogicalTemplateSpec("image"),
	})
	require.Error(t, err)
	assert.Equal(t, managererrors.ErrorConflict, managererrors.GetErrCode(err))
}

func TestGetTemplateRejectsStorageFailures(t *testing.T) {
	logical := &agentsv1alpha1.SandboxTemplate{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "team-a", Name: "template", UID: "template-uid",
			Annotations: map[string]string{agentsv1alpha1.AnnotationTemplateBuildState: agentsv1alpha1.TemplateBuildStateSubmitted},
		},
		Spec: validLogicalTemplateSpec("image"),
	}
	tests := []struct {
		name     string
		store    *templateStoreFuncs
		wantCode managererrors.ErrorCode
	}{
		{name: "missing template", store: stubStore(nil, templateNotFound("template")), wantCode: managererrors.ErrorNotFound},
		{name: "conflict checker unavailable", store: stubStore(logical, nil), wantCode: managererrors.ErrorUnavailable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			manager := &SandboxManager{templateStore: tt.store}
			_, err := manager.GetTemplate(t.Context(), "team-a", "template")
			require.Error(t, err)
			assert.Equal(t, tt.wantCode, managererrors.GetErrCode(err))
		})
	}
}

func TestSubmitTemplateRejectsInvalidSubmissions(t *testing.T) {
	reserve := ReserveTemplateOptions{Namespace: "team-a", Name: "template", CPUCount: 1, MemoryMB: 1024}
	draft := reservedTemplate(reserve, agentsv1alpha1.SandboxTemplateBackendKubernetes)
	draft.UID = "build-uid"
	draft.CreationTimestamp = metav1.Now()

	substrateDraft := reservedTemplate(reserve, agentsv1alpha1.SandboxTemplateBackendSubstrate)
	substrateDraft.UID = "build-uid"
	substrateDraft.CreationTimestamp = metav1.Now()

	draftWithoutWorkload := draft.DeepCopy()
	draftWithoutWorkload.Spec.Template = nil

	valueChange := submitEnvelopeSpec()
	valueChange.Template.Spec.Containers[0].Resources.Limits[corev1.ResourceMemory] = resource.MustParse("512Mi")

	substrateSpec := submitEnvelopeSpec()
	substrateSpec.Backend = agentsv1alpha1.SandboxTemplateBackendSubstrate

	tests := []struct {
		name     string
		template *agentsv1alpha1.SandboxTemplate
		getErr   error
		spec     agentsv1alpha1.SandboxTemplateSpec
		buildID  string
		checker  infra.LogicalTemplateNameConflictChecker
		wantCode managererrors.ErrorCode
	}{
		{name: "missing reservation", getErr: templateNotFound("template"), spec: submitEnvelopeSpec(), wantCode: managererrors.ErrorNotFound},
		{name: "read failure", getErr: errors.New("read boom"), spec: submitEnvelopeSpec(), wantCode: managererrors.ErrorInternal},
		{name: "build id mismatch", template: draft, spec: submitEnvelopeSpec(), buildID: "other-build", wantCode: managererrors.ErrorConflict},
		{name: "conflict checker failure", template: draft, spec: submitEnvelopeSpec(), checker: fixedTemplateNameConflictChecker{err: errors.New("checker boom")}, wantCode: managererrors.ErrorInternal},
		{name: "unsupported backend", template: substrateDraft, spec: substrateSpec, wantCode: managererrors.ErrorNotImplemented},
		{name: "draft without workload", template: draftWithoutWorkload, spec: submitEnvelopeSpec(), wantCode: managererrors.ErrorConflict},
		{name: "envelope is required", template: draft, spec: validLogicalTemplateSpec("image"), wantCode: managererrors.ErrorConflict},
		{name: "envelope change is rejected", template: draft, spec: valueChange, wantCode: managererrors.ErrorConflict},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			manager := &SandboxManager{templateStore: stubStore(tt.template, tt.getErr), templateNameConflictChecker: fixedTemplateNameConflictChecker{}}
			if tt.checker != nil {
				manager.templateNameConflictChecker = tt.checker
			}
			buildID := tt.buildID
			if buildID == "" {
				buildID = "build-uid"
			}
			_, err := manager.SubmitTemplate(t.Context(), SubmitTemplateOptions{
				Namespace: "team-a", Name: "template", BuildID: buildID, Spec: tt.spec,
			})
			require.Error(t, err)
			assert.Equal(t, tt.wantCode, managererrors.GetErrCode(err))
		})
	}
}

func TestSubmitTemplateConflictRecoveryRejectsAmbiguousFreshState(t *testing.T) {
	reserve := ReserveTemplateOptions{Namespace: "team-a", Name: "template", CPUCount: 1, MemoryMB: 1024}
	stale := reservedTemplate(reserve, agentsv1alpha1.SandboxTemplateBackendKubernetes)
	stale.UID = "build-uid"
	stale.CreationTimestamp = metav1.Now()
	submitSpec := submitEnvelopeSpec()
	digest, err := templateInputDigest(submitSpec)
	require.NoError(t, err)

	expired := stale.DeepCopy()
	expired.CreationTimestamp = metav1.NewTime(time.Now().Add(-2 * agentsv1alpha1.SandboxTemplateBuildReservationTTL))
	rebound := stale.DeepCopy()
	rebound.UID = "other-uid"
	rebound.Annotations = submittedTemplateAnnotations(digest)

	tests := []struct {
		name     string
		fresh    func(context.Context, string, string) (*agentsv1alpha1.SandboxTemplate, error)
		checker  infra.LogicalTemplateNameConflictChecker
		wantCode managererrors.ErrorCode
	}{
		{name: "fresh object vanished", fresh: stubRead(nil, templateNotFound("template")), wantCode: managererrors.ErrorNotFound},
		{name: "fresh read failure", fresh: stubRead(nil, errors.New("read boom")), wantCode: managererrors.ErrorInternal},
		{name: "fresh object ambiguity check failure", fresh: stubRead(stale, nil), checker: &flakyTemplateNameConflictChecker{err: errors.New("checker boom")}, wantCode: managererrors.ErrorInternal},
		{name: "fresh object bound to another build", fresh: stubRead(rebound, nil), wantCode: managererrors.ErrorConflict},
		{name: "fresh object expired while submitting", fresh: stubRead(expired, nil), wantCode: managererrors.ErrorNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := stubStore(stale, nil).withUpdate(apierrors.NewConflict(schema.GroupResource{Group: agentsv1alpha1.GroupVersion.Group, Resource: "sandboxtemplates"}, "template", errors.New("stale resourceVersion")))
			store.getFresh = tt.fresh
			manager := &SandboxManager{templateStore: store, templateNameConflictChecker: fixedTemplateNameConflictChecker{}}
			if tt.checker != nil {
				manager.templateNameConflictChecker = tt.checker
			}
			_, err := manager.SubmitTemplate(t.Context(), SubmitTemplateOptions{
				Namespace: "team-a", Name: "template", BuildID: "build-uid", Spec: submitSpec,
			})
			require.Error(t, err)
			assert.Equal(t, tt.wantCode, managererrors.GetErrCode(err))
		})
	}
}

func TestReclaimExpiredReservationRejectsStorageFailures(t *testing.T) {
	reserve := ReserveTemplateOptions{Namespace: "team-a", Name: "template", CPUCount: 1, MemoryMB: 1024}
	expired := reservedTemplate(reserve, agentsv1alpha1.SandboxTemplateBackendKubernetes)
	expired.UID = "expired-uid"
	expired.CreationTimestamp = metav1.NewTime(time.Now().Add(-2 * agentsv1alpha1.SandboxTemplateBuildReservationTTL))
	rebound := expired.DeepCopy()
	rebound.UID = "other-uid"

	tests := []struct {
		name      string
		deleteErr error
		fresh     func(context.Context, string, string) (*agentsv1alpha1.SandboxTemplate, error)
		checker   infra.LogicalTemplateNameConflictChecker
		wantCode  managererrors.ErrorCode
	}{
		{name: "reclaim delete failure", deleteErr: errors.New("delete boom"), fresh: stubRead(expired, nil), wantCode: managererrors.ErrorInternal},
		{name: "reclaim confirmation read failure", fresh: stubRead(nil, errors.New("read boom")), wantCode: managererrors.ErrorInternal},
		{name: "name rebound while reclaiming", fresh: stubRead(rebound, nil), wantCode: managererrors.ErrorUnavailable},
		{name: "backend name conflict after reclaim", fresh: stubRead(nil, templateNotFound("template")), checker: fixedTemplateNameConflictChecker{conflict: infra.LogicalTemplateNameConflictSandboxSet}, wantCode: managererrors.ErrorConflict},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := stubStore(expired, nil).withDelete(tt.deleteErr)
			store.getFresh = tt.fresh
			manager := &SandboxManager{templateStore: store, templateNameConflictChecker: fixedTemplateNameConflictChecker{}}
			if tt.checker != nil {
				manager.templateNameConflictChecker = tt.checker
			}
			_, err := manager.ReserveTemplate(t.Context(), reserve)
			require.Error(t, err)
			assert.Equal(t, tt.wantCode, managererrors.GetErrCode(err))
		})
	}
}

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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apiMeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	managererrors "github.com/openkruise/agents/pkg/sandbox-manager/errors"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
)

// CreateTemplateOptions is the protocol-neutral input for atomic logical
// Template creation. Namespace is resolved by the API layer from the caller.
type CreateTemplateOptions struct {
	Namespace string
	Name      string
	Spec      agentsv1alpha1.SandboxTemplateSpec
}

// ReserveTemplateOptions holds a logical name and resource envelope for a
// later two-phase submission.
type ReserveTemplateOptions struct {
	Namespace string
	Name      string
	Backend   string
	CPUCount  int32
	MemoryMB  int32
}

// SubmitTemplateOptions completes one reserved draft in place.
type SubmitTemplateOptions struct {
	Namespace string
	Name      string
	BuildID   string
	Spec      agentsv1alpha1.SandboxTemplateSpec
}

// TemplateDetails is the Manager projection shared by custom and compatible
// API surfaces.
type TemplateDetails struct {
	Name              string
	BuildID           string
	CreationTimestamp metav1.Time
	Spec              agentsv1alpha1.SandboxTemplateSpec
	Status            agentsv1alpha1.SandboxTemplateStatus
}

// CreateTemplate atomically creates a submitted logical Template. It returns
// created=false for an idempotent replay of the same normalized input.
func (m *SandboxManager) CreateTemplate(ctx context.Context, opts CreateTemplateOptions) (*TemplateDetails, bool, error) {
	if err := validateTemplateIdentity(opts.Namespace, opts.Name); err != nil {
		return nil, false, err
	}
	if err := normalizeAndValidateTemplateSpec(&opts.Spec); err != nil {
		return nil, false, err
	}
	store, err := m.templateStoreOrErr()
	if err != nil {
		return nil, false, err
	}
	digest, err := templateInputDigest(opts.Spec)
	if err != nil {
		return nil, false, managererrors.WrapError(managererrors.ErrorInternal, err, "calculate template input digest")
	}

	for attempt := 0; attempt < 2; attempt++ {
		existing, err := m.resolveTemplateNameForWrite(ctx, store, opts.Namespace, opts.Name, attempt)
		if err != nil {
			return nil, false, err
		}
		if existing != nil {
			return replayCreatedTemplate(existing, digest)
		}
		template := &agentsv1alpha1.SandboxTemplate{
			ObjectMeta: metav1.ObjectMeta{
				Namespace:   opts.Namespace,
				Name:        opts.Name,
				Annotations: submittedTemplateAnnotations(digest),
			},
			Spec: *opts.Spec.DeepCopy(),
		}
		if err := store.Create(ctx, template); err != nil {
			if apierrors.IsAlreadyExists(err) && attempt == 0 {
				continue
			}
			return nil, false, templateStorageError("create SandboxTemplate", err)
		}
		if template.UID == "" {
			return nil, false, managererrors.NewError(managererrors.ErrorInternal, "created SandboxTemplate has no uid")
		}
		return projectTemplate(template), true, nil
	}
	return nil, false, managererrors.NewError(managererrors.ErrorUnavailable, "template name %q changed concurrently; retry", opts.Name)
}

// ReserveTemplate creates or replays one reserved draft. The SandboxTemplate
// UID is the Build identity.
func (m *SandboxManager) ReserveTemplate(ctx context.Context, opts ReserveTemplateOptions) (*TemplateDetails, error) {
	if err := validateTemplateIdentity(opts.Namespace, opts.Name); err != nil {
		return nil, err
	}
	backend := opts.Backend
	if backend == "" {
		backend = agentsv1alpha1.SandboxTemplateBackendKubernetes
	}
	if backend != agentsv1alpha1.SandboxTemplateBackendKubernetes {
		return nil, managererrors.NewError(managererrors.ErrorNotImplemented, "backend %q is not configured", backend)
	}
	if opts.CPUCount <= 0 || opts.MemoryMB <= 0 {
		return nil, managererrors.NewError(managererrors.ErrorBadRequest, "cpuCount and memoryMB must be greater than zero")
	}
	store, err := m.templateStoreOrErr()
	if err != nil {
		return nil, err
	}

	for attempt := 0; attempt < 2; attempt++ {
		existing, err := m.resolveTemplateNameForWrite(ctx, store, opts.Namespace, opts.Name, attempt)
		if err != nil {
			return nil, err
		}
		if existing != nil {
			return reserveExistingTemplate(existing, opts, backend)
		}
		template := reservedTemplate(opts, backend)
		if err := store.Create(ctx, template); err != nil {
			if apierrors.IsAlreadyExists(err) && attempt == 0 {
				continue
			}
			return nil, templateStorageError("reserve SandboxTemplate", err)
		}
		if template.UID == "" {
			return nil, managererrors.NewError(managererrors.ErrorInternal, "reserved SandboxTemplate has no uid")
		}
		return projectTemplate(template), nil
	}
	return nil, managererrors.NewError(managererrors.ErrorUnavailable, "template name %q changed concurrently; retry", opts.Name)
}

// SubmitTemplate completes a reserved draft. Replaying the same Build and
// normalized input succeeds; a different Build or input conflicts.
func (m *SandboxManager) SubmitTemplate(ctx context.Context, opts SubmitTemplateOptions) (*TemplateDetails, error) {
	if err := validateTemplateIdentity(opts.Namespace, opts.Name); err != nil {
		return nil, err
	}
	if opts.BuildID == "" {
		return nil, managererrors.NewError(managererrors.ErrorBadRequest, "buildID is required")
	}
	store, err := m.templateStoreOrErr()
	if err != nil {
		return nil, err
	}
	template, err := store.Get(ctx, opts.Namespace, opts.Name)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil, managererrors.NewError(managererrors.ErrorNotFound, "template reservation %q not found", opts.Name)
		}
		return nil, templateStorageError("get Template reservation", err)
	}
	if err := m.ensureTemplateUnambiguous(ctx, template); err != nil {
		return nil, err
	}
	if string(template.UID) != opts.BuildID {
		return nil, managererrors.NewError(managererrors.ErrorConflict, "template %q is bound to build %q", opts.Name, template.UID)
	}
	state := template.Annotations[agentsv1alpha1.AnnotationTemplateBuildState]
	if state == agentsv1alpha1.TemplateBuildStateReserved {
		if reservationExpired(template, time.Now()) {
			return nil, managererrors.NewError(managererrors.ErrorNotFound, "template reservation %q has expired", opts.Name)
		}
		if template.Status.Phase == agentsv1alpha1.SandboxTemplatePhaseFailed {
			return nil, managererrors.NewError(managererrors.ErrorConflict, "template reservation %q has failed", opts.Name)
		}
	}

	backend := template.Spec.Backend
	if backend == "" {
		backend = agentsv1alpha1.SandboxTemplateBackendKubernetes
	}
	if opts.Spec.Backend == "" {
		opts.Spec.Backend = backend
	} else if opts.Spec.Backend != backend {
		return nil, managererrors.NewError(managererrors.ErrorConflict,
			"submitted backend %q does not match reserved backend %q", opts.Spec.Backend, backend)
	}
	if err := normalizeAndValidateTemplateSpec(&opts.Spec); err != nil {
		return nil, err
	}
	if !reservedEnvelopeMatches(template, opts.Spec.Template) {
		return nil, managererrors.NewError(managererrors.ErrorConflict, "submitted workload changes the reserved cpu or memory envelope")
	}
	digest, err := templateInputDigest(opts.Spec)
	if err != nil {
		return nil, managererrors.WrapError(managererrors.ErrorInternal, err, "calculate template input digest")
	}
	if details, replayed, replayErr := replaySubmittedTemplate(template, opts.BuildID, digest); replayed || replayErr != nil {
		return details, replayErr
	}

	updated := template.DeepCopy()
	updated.Spec = *opts.Spec.DeepCopy()
	if updated.Annotations == nil {
		updated.Annotations = map[string]string{}
	}
	updated.Annotations[agentsv1alpha1.AnnotationTemplateBuildInputDigest] = digest
	updated.Annotations[agentsv1alpha1.AnnotationTemplateBuildState] = agentsv1alpha1.TemplateBuildStateSubmitted
	if err := store.Update(ctx, updated); err != nil {
		if !apierrors.IsConflict(err) {
			return nil, templateStorageError("submit SandboxTemplate", err)
		}
		latest, getErr := store.GetFresh(ctx, opts.Namespace, opts.Name)
		if getErr != nil {
			if apierrors.IsNotFound(getErr) {
				return nil, managererrors.NewError(managererrors.ErrorNotFound, "template reservation %q not found", opts.Name)
			}
			return nil, templateStorageError("resolve concurrent Template submission", getErr)
		}
		if err := m.ensureTemplateUnambiguous(ctx, latest); err != nil {
			return nil, err
		}
		if details, replayed, replayErr := replaySubmittedTemplate(latest, opts.BuildID, digest); replayed || replayErr != nil {
			return details, replayErr
		}
		return nil, templateStorageError("submit SandboxTemplate", err)
	}
	return projectTemplate(updated), nil
}

// GetTemplate returns one logical Template by name.
func (m *SandboxManager) GetTemplate(ctx context.Context, namespace, name string) (*TemplateDetails, error) {
	if err := validateTemplateIdentity(namespace, name); err != nil {
		return nil, err
	}
	store, err := m.templateStoreOrErr()
	if err != nil {
		return nil, err
	}
	template, err := store.Get(ctx, namespace, name)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil, managererrors.NewError(managererrors.ErrorNotFound, "template %q not found", name)
		}
		return nil, templateStorageError("get SandboxTemplate", err)
	}
	state, logical := templateBuildState(template)
	if !logical || (state == agentsv1alpha1.TemplateBuildStateReserved && reservationExpired(template, time.Now())) {
		return nil, managererrors.NewError(managererrors.ErrorNotFound, "template %q not found", name)
	}
	if err := m.ensureTemplateUnambiguous(ctx, template); err != nil {
		return nil, err
	}
	return projectTemplate(template), nil
}

func (m *SandboxManager) templateStoreOrErr() (TemplateStore, error) {
	if m.templateStore == nil {
		return nil, managererrors.NewError(managererrors.ErrorUnavailable, "logical Template storage is unavailable")
	}
	return m.templateStore, nil
}

func (m *SandboxManager) resolveTemplateNameForWrite(
	ctx context.Context,
	store TemplateStore,
	namespace, name string,
	attempt int,
) (*agentsv1alpha1.SandboxTemplate, error) {
	existing, err := getTemplateForAttempt(ctx, store, namespace, name, attempt)
	if apierrors.IsNotFound(err) {
		if err := m.ensureTemplateNameAvailable(ctx, namespace, name); err != nil {
			return nil, err
		}
		return nil, nil
	}
	if err != nil {
		return nil, templateStorageError("get SandboxTemplate", err)
	}
	reclaimed, err := m.reclaimExpiredReservation(ctx, store, existing)
	if err != nil {
		return nil, err
	}
	if reclaimed {
		if err := m.ensureTemplateNameAvailable(ctx, namespace, name); err != nil {
			return nil, err
		}
		return nil, nil
	}
	if err := m.ensureTemplateUnambiguous(ctx, existing); err != nil {
		return nil, err
	}
	return existing, nil
}

func getTemplateForAttempt(ctx context.Context, store TemplateStore, namespace, name string, attempt int) (*agentsv1alpha1.SandboxTemplate, error) {
	if attempt == 0 {
		return store.Get(ctx, namespace, name)
	}
	return store.GetFresh(ctx, namespace, name)
}

func validateTemplateIdentity(namespace, name string) error {
	if namespace == "" {
		return managererrors.NewError(managererrors.ErrorBadRequest, "namespace is required")
	}
	if name == "" {
		return managererrors.NewError(managererrors.ErrorBadRequest, "template name is required")
	}
	if name == "tags" {
		return managererrors.NewError(managererrors.ErrorBadRequest, "template name %q is reserved", name)
	}
	return nil
}

func normalizeAndValidateTemplateSpec(spec *agentsv1alpha1.SandboxTemplateSpec) error {
	if spec.Backend == "" {
		spec.Backend = agentsv1alpha1.SandboxTemplateBackendKubernetes
	}
	if spec.Backend != agentsv1alpha1.SandboxTemplateBackendKubernetes {
		return managererrors.NewError(managererrors.ErrorNotImplemented, "backend %q is not configured", spec.Backend)
	}
	if spec.Template == nil {
		return managererrors.NewError(managererrors.ErrorBadRequest, "spec.template is required")
	}
	if len(spec.Template.Spec.Containers) == 0 {
		return managererrors.NewError(managererrors.ErrorBadRequest, "spec.template.spec.containers must not be empty")
	}
	for i := range spec.Template.Spec.Containers {
		if strings.TrimSpace(spec.Template.Spec.Containers[i].Image) == "" {
			return managererrors.NewError(managererrors.ErrorBadRequest, "spec.template.spec.containers[%d].image is required", i)
		}
	}
	return nil
}

func (m *SandboxManager) ensureTemplateNameAvailable(ctx context.Context, namespace, name string) error {
	conflict, err := m.logicalTemplateNameConflict(ctx, namespace, name)
	if err != nil {
		return err
	}
	if conflict != infra.LogicalTemplateNameConflictNone {
		return managererrors.NewError(managererrors.ErrorConflict, "%s %q already exists", conflict, name)
	}
	return nil
}

func (m *SandboxManager) ensureTemplateUnambiguous(ctx context.Context, template *agentsv1alpha1.SandboxTemplate) error {
	conflict, err := m.logicalTemplateNameConflict(ctx, template.Namespace, template.Name)
	if err != nil {
		return err
	}
	if conflict != infra.LogicalTemplateNameConflictNone {
		return managererrors.NewError(managererrors.ErrorConflict,
			"template %q conflicts with a same-named %s", template.Name, conflict)
	}
	return nil
}

func (m *SandboxManager) logicalTemplateNameConflict(ctx context.Context, namespace, name string) (infra.LogicalTemplateNameConflict, error) {
	if m.templateNameConflictChecker == nil {
		return infra.LogicalTemplateNameConflictNone,
			managererrors.NewError(managererrors.ErrorUnavailable, "logical Template name conflict checker is unavailable")
	}
	conflict, err := m.templateNameConflictChecker.CheckLogicalTemplateNameConflict(ctx, infra.CheckLogicalTemplateNameConflictOptions{
		Namespace: namespace,
		Name:      name,
	})
	if err != nil {
		return infra.LogicalTemplateNameConflictNone, templateStorageError("check template name conflict", err)
	}
	return conflict, nil
}

func replayCreatedTemplate(existing *agentsv1alpha1.SandboxTemplate, digest string) (*TemplateDetails, bool, error) {
	if len(existing.OwnerReferences) != 0 || existing.Annotations[agentsv1alpha1.AnnotationTemplateBuildState] != agentsv1alpha1.TemplateBuildStateSubmitted {
		return nil, false, managererrors.NewError(managererrors.ErrorConflict, "SandboxTemplate %q already exists", existing.Name)
	}
	if existing.Annotations[agentsv1alpha1.AnnotationTemplateBuildInputDigest] != digest {
		return nil, false, managererrors.NewError(managererrors.ErrorConflict, "template %q already exists with a different input", existing.Name)
	}
	return projectTemplate(existing), false, nil
}

func replaySubmittedTemplate(template *agentsv1alpha1.SandboxTemplate, buildID, digest string) (*TemplateDetails, bool, error) {
	if string(template.UID) != buildID {
		return nil, true, managererrors.NewError(managererrors.ErrorConflict,
			"template %q is bound to build %q", template.Name, template.UID)
	}
	switch template.Annotations[agentsv1alpha1.AnnotationTemplateBuildState] {
	case agentsv1alpha1.TemplateBuildStateSubmitted:
		if template.Annotations[agentsv1alpha1.AnnotationTemplateBuildInputDigest] != digest {
			return nil, true, managererrors.NewError(managererrors.ErrorConflict, "template build was submitted with a different input")
		}
		return projectTemplate(template), true, nil
	case agentsv1alpha1.TemplateBuildStateReserved:
		if reservationExpired(template, time.Now()) {
			return nil, true, managererrors.NewError(managererrors.ErrorNotFound, "template reservation %q has expired", template.Name)
		}
		return nil, false, nil
	default:
		return nil, true, managererrors.NewError(managererrors.ErrorConflict,
			"template has unexpected build state %q", template.Annotations[agentsv1alpha1.AnnotationTemplateBuildState])
	}
}

func reserveExistingTemplate(existing *agentsv1alpha1.SandboxTemplate, opts ReserveTemplateOptions, backend string) (*TemplateDetails, error) {
	if existing.Annotations[agentsv1alpha1.AnnotationTemplateBuildState] == agentsv1alpha1.TemplateBuildStateReserved &&
		existing.Status.Phase != agentsv1alpha1.SandboxTemplatePhaseFailed && reservationMatches(existing, opts, backend) {
		return projectTemplate(existing), nil
	}
	return nil, managererrors.NewError(managererrors.ErrorConflict, "template name %q is already bound", existing.Name)
}

func (m *SandboxManager) reclaimExpiredReservation(ctx context.Context, store TemplateStore, existing *agentsv1alpha1.SandboxTemplate) (bool, error) {
	state, logical := templateBuildState(existing)
	if !logical || state != agentsv1alpha1.TemplateBuildStateReserved || !reservationExpired(existing, time.Now()) {
		return false, nil
	}
	if err := store.Delete(ctx, existing.Namespace, existing.Name, existing.UID, existing.ResourceVersion); err != nil && !apierrors.IsNotFound(err) {
		return false, templateStorageError("reclaim expired Template reservation", err)
	}
	latest, err := store.GetFresh(ctx, existing.Namespace, existing.Name)
	if apierrors.IsNotFound(err) {
		return true, nil
	}
	if err != nil {
		return false, templateStorageError("confirm expired Template reservation deletion", err)
	}
	if latest.UID != existing.UID {
		return false, managererrors.NewError(managererrors.ErrorUnavailable,
			"template name %q was rebound while reclaiming an expired reservation; retry", existing.Name)
	}
	return false, managererrors.NewError(managererrors.ErrorUnavailable,
		"expired template reservation %q is still terminating; retry", existing.Name)
}

func templateBuildState(template *agentsv1alpha1.SandboxTemplate) (string, bool) {
	if len(template.OwnerReferences) != 0 || template.Annotations == nil {
		return "", false
	}
	state, ok := template.Annotations[agentsv1alpha1.AnnotationTemplateBuildState]
	return state, ok
}

func reservationExpired(template *agentsv1alpha1.SandboxTemplate, now time.Time) bool {
	if !now.Before(template.CreationTimestamp.Add(agentsv1alpha1.SandboxTemplateBuildReservationTTL)) {
		return true
	}
	condition := apiMeta.FindStatusCondition(template.Status.Conditions, agentsv1alpha1.SandboxTemplateBuildConditionReady)
	return template.Status.Phase == agentsv1alpha1.SandboxTemplatePhaseFailed && condition != nil &&
		condition.Reason == agentsv1alpha1.SandboxTemplateBuildReasonReservationExpired
}

func reservedTemplate(opts ReserveTemplateOptions, backend string) *agentsv1alpha1.SandboxTemplate {
	cpu := resource.MustParse(strconv.FormatInt(int64(opts.CPUCount), 10))
	memory := resource.MustParse(fmt.Sprintf("%dMi", opts.MemoryMB))
	resources := corev1.ResourceList{corev1.ResourceCPU: cpu, corev1.ResourceMemory: memory}
	annotations := map[string]string{agentsv1alpha1.AnnotationTemplateBuildState: agentsv1alpha1.TemplateBuildStateReserved}
	return &agentsv1alpha1.SandboxTemplate{
		ObjectMeta: metav1.ObjectMeta{Namespace: opts.Namespace, Name: opts.Name, Annotations: annotations},
		Spec: agentsv1alpha1.SandboxTemplateSpec{
			Backend: backend,
			Template: &corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{
				Name:      "runtime",
				Resources: corev1.ResourceRequirements{Requests: resources.DeepCopy(), Limits: resources.DeepCopy()},
			}}}},
		},
	}
}

func reservationMatches(template *agentsv1alpha1.SandboxTemplate, opts ReserveTemplateOptions, backend string) bool {
	if template.Spec.Backend != backend || template.Spec.Template == nil || len(template.Spec.Template.Spec.Containers) != 1 {
		return false
	}
	container := template.Spec.Template.Spec.Containers[0]
	wantCPU := resource.MustParse(strconv.FormatInt(int64(opts.CPUCount), 10))
	wantMemory := resource.MustParse(fmt.Sprintf("%dMi", opts.MemoryMB))
	for _, resources := range []corev1.ResourceList{container.Resources.Requests, container.Resources.Limits} {
		cpu, cpuOK := resources[corev1.ResourceCPU]
		memory, memoryOK := resources[corev1.ResourceMemory]
		if !cpuOK || !memoryOK || !cpu.Equal(wantCPU) || !memory.Equal(wantMemory) {
			return false
		}
	}
	return true
}

func reservedEnvelopeMatches(draft *agentsv1alpha1.SandboxTemplate, submitted *corev1.PodTemplateSpec) bool {
	if draft.Spec.Template == nil || submitted == nil || len(draft.Spec.Template.Spec.Containers) != 1 || len(submitted.Spec.Containers) == 0 {
		return false
	}
	reserved := draft.Spec.Template.Spec.Containers[0].Resources
	actual := submitted.Spec.Containers[0].Resources
	return equalResourceList(reserved.Requests, actual.Requests) && equalResourceList(reserved.Limits, actual.Limits)
}

func equalResourceList(left, right corev1.ResourceList) bool {
	if len(left) != len(right) {
		return false
	}
	for name, leftQuantity := range left {
		rightQuantity, ok := right[name]
		if !ok || !leftQuantity.Equal(rightQuantity) {
			return false
		}
	}
	return true
}

func submittedTemplateAnnotations(digest string) map[string]string {
	return map[string]string{
		agentsv1alpha1.AnnotationTemplateBuildState:       agentsv1alpha1.TemplateBuildStateSubmitted,
		agentsv1alpha1.AnnotationTemplateBuildInputDigest: digest,
	}
}

func templateInputDigest(spec agentsv1alpha1.SandboxTemplateSpec) (string, error) {
	data, err := json.Marshal(spec)
	if err != nil {
		return "", fmt.Errorf("marshal normalized template input: %w", err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

func projectTemplate(template *agentsv1alpha1.SandboxTemplate) *TemplateDetails {
	status := *template.Status.DeepCopy()
	if status.Phase == "" {
		status.Phase = agentsv1alpha1.SandboxTemplatePhasePending
	}
	return &TemplateDetails{
		Name:              template.Name,
		BuildID:           string(template.UID),
		CreationTimestamp: template.CreationTimestamp,
		Spec:              *template.Spec.DeepCopy(),
		Status:            status,
	}
}

func templateStorageError(operation string, err error) error {
	switch {
	case apierrors.IsConflict(err), apierrors.IsAlreadyExists(err):
		return managererrors.WrapError(managererrors.ErrorConflict, err, "%s: %v", operation, err)
	case apierrors.IsInvalid(err), apierrors.IsBadRequest(err):
		return managererrors.WrapError(managererrors.ErrorBadRequest, err, "%s: %v", operation, err)
	case apierrors.IsNotFound(err):
		return managererrors.WrapError(managererrors.ErrorNotFound, err, "%s: %v", operation, err)
	case apierrors.IsTimeout(err), apierrors.IsServerTimeout(err), apierrors.IsTooManyRequests(err), apierrors.IsServiceUnavailable(err):
		return managererrors.WrapError(managererrors.ErrorUnavailable, err, "%s: %v", operation, err)
	default:
		return managererrors.WrapError(managererrors.ErrorInternal, err, "%s: %v", operation, err)
	}
}

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
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	apivalidation "k8s.io/apimachinery/pkg/api/validation"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	api "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
	"github.com/openkruise/agents/pkg/templateconfig"
	"github.com/openkruise/agents/pkg/templatecreation"
	"github.com/openkruise/agents/pkg/templateindex"
)

type templateStore struct {
	cached    client.Reader
	writer    client.Client
	live      templateKeyReader
	namespace string
	view      *templateView
}

func (s *templateStore) Run(ctx context.Context) error {
	if s.view == nil {
		return nil
	}
	return s.view.Run(ctx)
}
func (s *templateStore) Stop(ctx context.Context) {
	if s.view != nil {
		s.view.Stop(ctx)
	}
}
func (s *templateStore) check(ctx context.Context, ns string) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("%w: %w", infra.ErrTemplateViewUnavailable, err)
	}
	if ns == "" || s.namespace != "" && s.namespace != ns {
		return infra.ErrTemplateScope
	}
	if s.view != nil && !s.view.Healthy() {
		return infra.ErrTemplateViewUnavailable
	}
	return nil
}
func templateStoreError(err error) error {
	if err == nil {
		return nil
	}
	switch {
	case apierrors.IsNotFound(err):
		return fmt.Errorf("%w: %w", infra.ErrTemplateMissing, err)
	case apierrors.IsAlreadyExists(err):
		return fmt.Errorf("%w: %w", infra.ErrTemplateExists, err)
	case apierrors.IsInvalid(err), apierrors.IsBadRequest(err):
		return fmt.Errorf("%w: %w", infra.ErrTemplateInvalid, err)
	case apierrors.IsConflict(err):
		return fmt.Errorf("%w: %w", infra.ErrTemplateIdentityChanged, err)
	case apierrors.IsTimeout(err), apierrors.IsServerTimeout(err), apierrors.IsTooManyRequests(err), apierrors.IsServiceUnavailable(err), errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		return fmt.Errorf("%w: %w", infra.ErrTemplateViewUnavailable, err)
	default:
		return fmt.Errorf("template backend operation: %w", err)
	}
}
func (s *templateStore) CreateClaim(ctx context.Context, ns, name, id string, metadata templateconfig.Metadata) (infra.TemplateClaimIdentity, error) {
	if err := s.check(ctx, ns); err != nil {
		return infra.TemplateClaimIdentity{}, err
	}
	metadata = metadata.Clone()
	if metadata.Annotations == nil {
		metadata.Annotations = map[string]string{}
	}
	metadata.Annotations[templatecreation.AllocationAnnotation] = id
	// Reserve the completion marker before the first write, so a full user map
	// cannot prevent the creator from publishing positive completion evidence.
	metadata.Annotations[templatecreation.CreationCompleteAnnotation] = "true"
	if err := apivalidation.ValidateAnnotationsSize(metadata.Annotations); err != nil {
		return infra.TemplateClaimIdentity{}, fmt.Errorf("%w: %w", infra.ErrTemplateInvalid, err)
	}
	delete(metadata.Annotations, templatecreation.CreationCompleteAnnotation)
	c := &api.SandboxTemplateClaim{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name,
		Finalizers: []string{api.SandboxTemplateClaimCleanupFinalizer}, Labels: metadata.Labels, Annotations: metadata.Annotations}}
	if err := s.writer.Create(ctx, c, &client.CreateOptions{FieldValidation: metav1.FieldValidationStrict}); err != nil {
		if apierrors.IsNotFound(err) {
			return infra.TemplateClaimIdentity{}, fmt.Errorf("%w: %w", infra.ErrTemplateViewUnavailable, err)
		}
		return infra.TemplateClaimIdentity{}, templateStoreError(err)
	}
	return infra.TemplateClaimIdentity{Namespace: ns, Name: name, UID: string(c.UID)}, nil
}
func (s *templateStore) CreateVersion(ctx context.Context, c infra.TemplateClaimIdentity, id string, d templateconfig.Definition) error {
	if err := s.check(ctx, c.Namespace); err != nil {
		return err
	}
	current := &api.SandboxTemplateClaim{}
	if err := s.live.Get(ctx, client.ObjectKey{Namespace: c.Namespace, Name: c.Name}, current); err != nil {
		return templateStoreError(err)
	}
	if string(current.UID) != c.UID || current.DeletionTimestamp != nil {
		return infra.ErrTemplateIdentityChanged
	}
	deadline := current.CreationTimestamp.Add(templatecreation.CreationRequestBudget)
	if current.CreationTimestamp.IsZero() {
		deadline = time.Now().Add(templatecreation.CreationRequestBudget)
	}
	writeCtx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	if err := writeCtx.Err(); err != nil {
		return templateStoreError(err)
	}
	metadata := d.Metadata.Clone()
	v := &api.SandboxTemplate{ObjectMeta: metav1.ObjectMeta{Namespace: c.Namespace, Name: id, Labels: metadata.Labels, Annotations: metadata.Annotations,
		OwnerReferences: []metav1.OwnerReference{{APIVersion: api.GroupVersion.String(), Kind: "SandboxTemplateClaim", Name: c.Name, UID: types.UID(c.UID), Controller: ptr.To(true), BlockOwnerDeletion: ptr.To(true)}}}, Spec: *d.TemplateSpec()}
	err := s.writer.Create(writeCtx, v, &client.CreateOptions{FieldValidation: metav1.FieldValidationStrict})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Errorf("%w: %w", infra.ErrTemplateViewUnavailable, err)
		}
		return templateStoreError(err)
	}
	// This marker is only positive evidence after a successful single Create.
	// Ambiguous failures retain the bounded empty-creation grace instead.
	before := current.DeepCopy()
	if current.Annotations == nil {
		current.Annotations = map[string]string{}
	}
	current.Annotations[templatecreation.CreationCompleteAnnotation] = "true"
	if err := s.writer.Patch(writeCtx, current, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
		klog.FromContext(ctx).Error(err, "template creation completion marker not published; deletion retains its bounded grace")
	}
	return nil
}
func (s *templateStore) GetClaim(ctx context.Context, ns, name string) (*infra.TemplateClaimObservation, error) {
	if err := s.check(ctx, ns); err != nil {
		return nil, err
	}
	c := &api.SandboxTemplateClaim{}
	if err := s.live.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, c); err != nil {
		return nil, templateStoreError(err)
	}
	return &infra.TemplateClaimObservation{Identity: infra.TemplateClaimIdentity{Namespace: c.Namespace, Name: c.Name, UID: string(c.UID)}, Deleting: c.DeletionTimestamp != nil, Metadata: templateconfig.UserMetadata(c.Labels, c.Annotations)}, nil
}
func (s *templateStore) ListClaims(ctx context.Context, ns string) ([]infra.TemplateClaimObservation, error) {
	if err := s.check(ctx, ns); err != nil {
		return nil, err
	}
	list := &api.SandboxTemplateClaimList{}
	if err := s.cached.List(ctx, list, client.InNamespace(ns)); err != nil {
		return nil, templateStoreError(err)
	}
	out := make([]infra.TemplateClaimObservation, 0, len(list.Items))
	for _, c := range list.Items {
		out = append(out, infra.TemplateClaimObservation{Identity: infra.TemplateClaimIdentity{Namespace: c.Namespace, Name: c.Name, UID: string(c.UID)}, Deleting: c.DeletionTimestamp != nil, Metadata: templateconfig.UserMetadata(c.Labels, c.Annotations)})
	}
	return out, nil
}
func (s *templateStore) ListVersions(ctx context.Context, c infra.TemplateClaimIdentity) ([]infra.TemplateVersionObservation, error) {
	if err := s.check(ctx, c.Namespace); err != nil {
		return nil, err
	}
	list := &api.SandboxTemplateList{}
	if err := s.cached.List(ctx, list, client.InNamespace(c.Namespace), client.MatchingFields{templateindex.ClaimOwner: c.UID}); err != nil {
		return nil, templateStoreError(err)
	}
	out := make([]infra.TemplateVersionObservation, 0, len(list.Items))
	for i := range list.Items {
		v := &list.Items[i]
		if ownedTemplate(v, c) {
			out = append(out, observeTemplateVersion(v))
		}
	}
	return out, nil
}
func ownedTemplate(v *api.SandboxTemplate, c infra.TemplateClaimIdentity) bool {
	o := templateindex.ClaimOwnerReference(v)
	return v.Namespace == c.Namespace && o != nil && o.Name == c.Name && string(o.UID) == c.UID
}
func observeTemplateVersion(v *api.SandboxTemplate) infra.TemplateVersionObservation {
	o := infra.TemplateVersionObservation{Metadata: templateconfig.UserMetadata(v.Labels, v.Annotations), ID: v.Name, Phase: string(api.SandboxTemplatePhasePending), CreatedAt: v.CreationTimestamp.Time, Deleting: v.DeletionTimestamp != nil}
	switch phase := templatecreation.DefinitionPhase(v); {
	case phase == api.SandboxTemplatePhaseFailed:
		o.Phase, o.Message = string(phase), meta.FindStatusCondition(v.Status.Conditions, templatecreation.DefinitionCondition).Message
	case phase != "":
		o.Phase = string(phase)
	case v.Status.Phase == api.SandboxTemplatePhaseBuilding:
		o.Phase = string(api.SandboxTemplatePhaseBuilding)
	}
	return o
}
func (s *templateStore) GetVersion(ctx context.Context, c infra.TemplateClaimIdentity, id string) (*infra.TemplateVersionObservation, error) {
	if err := s.check(ctx, c.Namespace); err != nil {
		return nil, err
	}
	v := &api.SandboxTemplate{}
	key := client.ObjectKey{Namespace: c.Namespace, Name: id}
	err := s.cached.Get(ctx, key, v)
	if err != nil && !apierrors.IsNotFound(err) {
		return nil, templateStoreError(err)
	}
	if apierrors.IsNotFound(err) || !ownedTemplate(v, c) {
		if err := s.live.Get(ctx, key, v); err != nil {
			return nil, templateStoreError(err)
		}
	}
	if !ownedTemplate(v, c) {
		return nil, infra.ErrTemplateMissing
	}
	o := observeTemplateVersion(v)
	return &o, nil
}

// managedVersions supplements only the known allocation key on negative cache
// observation. It is a cleanup ledger, never default-version selection.
func (s *templateStore) managedVersions(ctx context.Context, c infra.TemplateClaimIdentity) ([]api.SandboxTemplate, error) {
	list := &api.SandboxTemplateList{}
	if err := s.cached.List(ctx, list, client.InNamespace(c.Namespace), client.MatchingFields{templateindex.ClaimOwner: c.UID}); err != nil {
		return nil, templateStoreError(err)
	}
	out := []api.SandboxTemplate{}
	seen := map[string]bool{}
	for _, v := range list.Items {
		if ownedTemplate(&v, c) {
			out = append(out, v)
			seen[v.Name] = true
		}
	}
	current := &api.SandboxTemplateClaim{}
	if err := s.live.Get(ctx, client.ObjectKey{Namespace: c.Namespace, Name: c.Name}, current); err != nil {
		return nil, templateStoreError(err)
	}
	if string(current.UID) != c.UID {
		return nil, infra.ErrTemplateIdentityChanged
	}
	if id := current.Annotations[templatecreation.AllocationAnnotation]; id != "" && !seen[id] {
		v := &api.SandboxTemplate{}
		err := s.live.Get(ctx, client.ObjectKey{Namespace: c.Namespace, Name: id}, v)
		if err != nil && !apierrors.IsNotFound(err) {
			return nil, templateStoreError(err)
		}
		if err == nil && ownedTemplate(v, c) {
			out = append(out, *v)
		}
	}
	return out, nil
}
func (s *templateStore) HasExternalReferences(ctx context.Context, c infra.TemplateClaimIdentity) (bool, error) {
	if err := s.check(ctx, c.Namespace); err != nil {
		return false, err
	}
	versions, err := s.managedVersions(ctx, c)
	if err != nil {
		return false, err
	}
	for i := range versions {
		v := &versions[i]
		pools := &api.SandboxSetList{}
		if err := s.cached.List(ctx, pools, client.InNamespace(c.Namespace), client.MatchingFields{templateindex.TemplateRef: v.Name}); err != nil {
			return false, templateStoreError(err)
		}
		for j := range pools.Items {
			p := &pools.Items[j]
			ref := p.Spec.TemplateRef
			if ref == nil || ref.Name != v.Name {
				continue
			}
			owner := templateindex.VersionOwnerReference(p)
			if p.Name != v.Name || owner == nil || owner.Name != v.Name || owner.UID != v.UID {
				return true, nil
			}
		}
	}
	return false, nil
}
func (s *templateStore) BeginDelete(ctx context.Context, c infra.TemplateClaimIdentity) error {
	if err := s.check(ctx, c.Namespace); err != nil {
		return err
	}
	o := &api.SandboxTemplateClaim{ObjectMeta: metav1.ObjectMeta{Namespace: c.Namespace, Name: c.Name}}
	err := s.writer.Delete(ctx, o, client.Preconditions{UID: ptr.To(types.UID(c.UID))}, client.PropagationPolicy(metav1.DeletePropagationBackground))
	if apierrors.IsNotFound(err) {
		return nil
	}
	return templateStoreError(err)
}
func (s *templateStore) Deleted(ctx context.Context, c infra.TemplateClaimIdentity) (bool, error) {
	if err := s.check(ctx, c.Namespace); err != nil {
		return false, err
	}
	key := client.ObjectKey{Namespace: c.Namespace, Name: c.Name}
	current := &api.SandboxTemplateClaim{}
	err := s.cached.Get(ctx, key, current)
	if err == nil && string(current.UID) == c.UID {
		return false, nil
	}
	if err != nil && !apierrors.IsNotFound(err) {
		return false, templateStoreError(err)
	}
	// A cached absence or replacement is confirmed live once. Normal controller
	// cleanup releases the parent last, and replacements never become targets.
	err = s.live.Get(ctx, key, current)
	if err != nil && !apierrors.IsNotFound(err) {
		return false, templateStoreError(err)
	}
	return apierrors.IsNotFound(err) || string(current.UID) != c.UID, nil
}

var _ infra.TemplateStore = (*templateStore)(nil)

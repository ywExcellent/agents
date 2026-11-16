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
	"crypto/rand"
	"errors"
	"math/big"
	"sort"
	"time"

	managererrors "github.com/openkruise/agents/pkg/sandbox-manager/errors"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
	"github.com/openkruise/agents/pkg/templateconfig"
)

// TemplateSummary is a business observation, not an HTTP response model.
type TemplateSummary struct{ Name, ID, Status, Message string }

func (m *SandboxManager) templateBackend() (infra.TemplateStore, error) {
	if p, ok := m.infra.(infra.TemplateStoreProvider); ok && p.GetTemplateStore() != nil {
		return p.GetTemplateStore(), nil
	}
	return nil, managererrors.NewError(managererrors.ErrorUnavailable, "Template capability is unavailable.")
}
func templateDomainError(err error) error {
	if err == nil {
		return nil
	}
	code, message := managererrors.ErrorInternal, "Template backend operation failed."
	switch {
	case errors.Is(err, infra.ErrTemplateMissing):
		code, message = managererrors.ErrorNotFound, "Template not found."
	case errors.Is(err, infra.ErrTemplateExists):
		code, message = managererrors.ErrorConflict, "Template name already exists."
	case errors.Is(err, infra.ErrTemplateScope):
		code, message = managererrors.ErrorNotAllowed, "Team is outside the supported template scope."
	case errors.Is(err, infra.ErrTemplateIdentityChanged):
		code, message = managererrors.ErrorConflict, "Template identity changed."
	case errors.Is(err, infra.ErrTemplateInvalid):
		code, message = managererrors.ErrorBadRequest, "Invalid template definition."
	case errors.Is(err, infra.ErrTemplateViewUnavailable):
		code, message = managererrors.ErrorUnavailable, "Template backend is temporarily unavailable."
	}
	return managererrors.WrapError(code, err, "%s", message)
}
func allocateTemplateID() (string, error) {
	const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
	out := make([]byte, 20)
	for i := range out {
		n, err := rand.Int(rand.Reader, big.NewInt(int64(len(alphabet))))
		if err != nil {
			return "", err
		}
		out[i] = alphabet[n.Int64()]
	}
	return string(out), nil
}
func (m *SandboxManager) CreateTemplate(ctx context.Context, ns, name string, d templateconfig.Definition) (TemplateSummary, error) {
	normalized, err := templateconfig.NormalizeDefinition(d)
	if err != nil {
		return TemplateSummary{}, managererrors.WrapError(managererrors.ErrorBadRequest, err, "Invalid template definition.")
	}
	store, err := m.templateBackend()
	if err != nil {
		return TemplateSummary{}, err
	}
	id, err := allocateTemplateID()
	if err != nil {
		return TemplateSummary{}, managererrors.WrapError(managererrors.ErrorInternal, err, "Unable to allocate template ID.")
	}
	claim, err := store.CreateClaim(ctx, ns, name, id)
	if err != nil {
		return TemplateSummary{}, templateDomainError(err)
	}
	if err := store.CreateVersion(ctx, claim, id, normalized); err != nil {
		domain := templateDomainError(err)
		return TemplateSummary{}, managererrors.WrapError(managererrors.GetErrCode(domain), err, "Template creation did not complete; query the name and explicitly delete partial resources before retrying.")
	}
	return TemplateSummary{Name: name, ID: id, Status: "Pending"}, nil
}
func templateSummary(c infra.TemplateClaimObservation, v *infra.TemplateVersionObservation) TemplateSummary {
	s := TemplateSummary{Name: c.Identity.Name, Status: "Unknown"}
	if v != nil {
		s.ID, s.Status, s.Message = v.ID, v.Phase, v.Message
	}
	if c.Deleting || v != nil && v.Deleting {
		s.Status, s.Message = "Deleting", ""
	}
	return s
}
func latestTemplateVersion(v []infra.TemplateVersionObservation) *infra.TemplateVersionObservation {
	if len(v) == 0 {
		return nil
	}
	sort.Slice(v, func(i, j int) bool {
		if v[i].CreatedAt.Equal(v[j].CreatedAt) {
			return v[i].ID > v[j].ID
		}
		return v[i].CreatedAt.After(v[j].CreatedAt)
	})
	return &v[0]
}
func (m *SandboxManager) GetTemplate(ctx context.Context, ns, name, id string) (TemplateSummary, error) {
	store, err := m.templateBackend()
	if err != nil {
		return TemplateSummary{}, err
	}
	c, err := store.GetClaim(ctx, ns, name)
	if err != nil {
		return TemplateSummary{}, templateDomainError(err)
	}
	var v *infra.TemplateVersionObservation
	if id != "" {
		v, err = store.GetVersion(ctx, c.Identity, id)
	} else {
		var versions []infra.TemplateVersionObservation
		versions, err = store.ListVersions(ctx, c.Identity)
		v = latestTemplateVersion(versions)
	}
	if err != nil {
		return TemplateSummary{}, templateDomainError(err)
	}
	return templateSummary(*c, v), nil
}
func (m *SandboxManager) ListTemplates(ctx context.Context, ns string) ([]TemplateSummary, error) {
	store, err := m.templateBackend()
	if err != nil {
		return nil, err
	}
	claims, err := store.ListClaims(ctx, ns)
	if err != nil {
		return nil, templateDomainError(err)
	}
	out := make([]TemplateSummary, 0, len(claims))
	for _, c := range claims {
		versions, err := store.ListVersions(ctx, c.Identity)
		if err != nil {
			return nil, templateDomainError(err)
		}
		out = append(out, templateSummary(c, latestTemplateVersion(versions)))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}
func (m *SandboxManager) DeleteTemplate(ctx context.Context, ns, name string) error {
	store, err := m.templateBackend()
	if err != nil {
		return err
	}
	c, err := store.GetClaim(ctx, ns, name)
	if errors.Is(err, infra.ErrTemplateMissing) {
		return nil
	}
	if err != nil {
		return templateDomainError(err)
	}
	if !c.Deleting {
		external, err := store.HasExternalReferences(ctx, c.Identity)
		if err != nil {
			return templateDomainError(err)
		}
		if external {
			return managererrors.NewError(managererrors.ErrorConflict, "Template is referenced by an external SandboxSet.")
		}
	}
	if err := store.BeginDelete(ctx, c.Identity); err != nil {
		if !errors.Is(err, infra.ErrTemplateIdentityChanged) {
			return templateDomainError(err)
		}
		// A replacement is not a target. Confirm the pinned incarnation is gone.
	}
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		done, err := store.Deleted(ctx, c.Identity)
		if err != nil {
			return templateDomainError(err)
		}
		if done {
			return nil
		}
		select {
		case <-ctx.Done():
			return managererrors.WrapError(managererrors.ErrorUnavailable, ctx.Err(), "Template deletion is still in progress.")
		case <-ticker.C:
		}
	}
}

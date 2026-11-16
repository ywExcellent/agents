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
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"

	managererrors "github.com/openkruise/agents/pkg/sandbox-manager/errors"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
	"github.com/openkruise/agents/pkg/templateconfig"
)

// The generic Sandbox backend has many unrelated capabilities. Embedding it
// isolates the optional template capability; only the latter is exercised.
type templateBusinessInfra struct {
	infra.Infrastructure
	store *templateBusinessStore
}

func (i *templateBusinessInfra) GetTemplateStore() infra.TemplateStore { return i.store }

type templateBusinessStore struct {
	infra.TemplateStore
	claim                    *infra.TemplateClaimObservation
	versions                 []infra.TemplateVersionObservation
	failVersion              error
	external, done           bool
	createCalls, deleteCalls int
	pinned                   []infra.TemplateClaimIdentity
	definition               templateconfig.Definition
}

func (s *templateBusinessStore) CreateClaim(_ context.Context, ns, name, _ string, metadata templateconfig.Metadata) (infra.TemplateClaimIdentity, error) {
	s.createCalls++
	if s.claim != nil {
		return infra.TemplateClaimIdentity{}, infra.ErrTemplateExists
	}
	s.claim = &infra.TemplateClaimObservation{Metadata: metadata.Clone(), Identity: infra.TemplateClaimIdentity{Namespace: ns, Name: name, UID: "claim-uid"}}
	return s.claim.Identity, nil
}
func (s *templateBusinessStore) CreateVersion(_ context.Context, _ infra.TemplateClaimIdentity, id string, d templateconfig.Definition) error {
	s.definition = d
	if s.failVersion != nil {
		return s.failVersion
	}
	s.versions = []infra.TemplateVersionObservation{{ID: id, Phase: "Ready", Metadata: d.Metadata.Clone()}}
	return nil
}
func (s *templateBusinessStore) GetClaim(context.Context, string, string) (*infra.TemplateClaimObservation, error) {
	if s.claim == nil {
		return nil, infra.ErrTemplateMissing
	}
	return s.claim, nil
}
func (s *templateBusinessStore) ListClaims(context.Context, string) ([]infra.TemplateClaimObservation, error) {
	if s.claim == nil {
		return []infra.TemplateClaimObservation{}, nil
	}
	return []infra.TemplateClaimObservation{*s.claim}, nil
}
func (s *templateBusinessStore) ListVersions(context.Context, infra.TemplateClaimIdentity) ([]infra.TemplateVersionObservation, error) {
	return append([]infra.TemplateVersionObservation(nil), s.versions...), nil
}
func (s *templateBusinessStore) GetVersion(_ context.Context, _ infra.TemplateClaimIdentity, id string) (*infra.TemplateVersionObservation, error) {
	for _, v := range s.versions {
		if v.ID == id {
			return &v, nil
		}
	}
	return nil, infra.ErrTemplateMissing
}
func (s *templateBusinessStore) HasExternalReferences(context.Context, infra.TemplateClaimIdentity) (bool, error) {
	return s.external, nil
}
func (s *templateBusinessStore) BeginDelete(_ context.Context, c infra.TemplateClaimIdentity) error {
	s.deleteCalls++
	s.pinned = append(s.pinned, c)
	return nil
}
func (s *templateBusinessStore) Deleted(_ context.Context, c infra.TemplateClaimIdentity) (bool, error) {
	s.pinned = append(s.pinned, c)
	return s.done, nil
}
func templateBusinessManager(s *templateBusinessStore) *SandboxManager {
	return &SandboxManager{infra: &templateBusinessInfra{store: s}}
}
func TestTemplateManagerCreationAndPartialFailure(t *testing.T) {
	for _, test := range []struct {
		name    string
		failure error
		want    managererrors.ErrorCode
	}{
		{name: "success always Pending"}, {name: "partial persists", failure: infra.ErrTemplateViewUnavailable, want: managererrors.ErrorUnavailable},
	} {
		t.Run(test.name, func(t *testing.T) {
			s := &templateBusinessStore{failVersion: test.failure}
			m := templateBusinessManager(s)
			metadata := templateconfig.Metadata{Labels: map[string]string{"project": "analysis"}, Annotations: map[string]string{"description": "worker"}}
			result, err := m.CreateTemplate(t.Context(), "team-a", "python-agent", templateconfig.Definition{Metadata: metadata, Pod: corev1.PodSpec{Containers: []corev1.Container{{Name: "main", Image: "python:3.12"}}}})
			if s.claim != nil && s.claim.Metadata.Labels["project"] != "analysis" {
				t.Fatal("claim metadata missing")
			}
			if test.failure != nil {
				if managererrors.GetErrCode(err) != test.want || s.claim == nil {
					t.Fatalf("error=%v claim=%v", err, s.claim)
				}
				_, again := m.CreateTemplate(t.Context(), "team-a", "python-agent", templateconfig.Definition{Pod: corev1.PodSpec{Containers: []corev1.Container{{Name: "main", Image: "python:3.12"}}}})
				if managererrors.GetErrCode(again) != managererrors.ErrorConflict {
					t.Fatalf("repeat=%v", again)
				}
				summary, getErr := m.GetTemplate(t.Context(), "team-a", "python-agent", "")
				if getErr != nil || summary.Status != "Unknown" || summary.ID != "" || summary.Metadata.Annotations["description"] != "worker" {
					t.Fatalf("summary=%+v error=%v", summary, getErr)
				}
				return
			}
			if err != nil || result.Metadata.Labels["project"] != "analysis" || result.Status != "Pending" || len(result.ID) != 20 || s.definition.WarmPool == nil || s.definition.WarmPool.Size == nil || *s.definition.WarmPool.Size != 0 {
				t.Fatalf("summary=%+v error=%v", result, err)
			}
			metadata.Labels["project"] = "changed"
			result.Metadata.Annotations["description"] = "changed"
			listed, err := m.ListTemplates(t.Context(), "team-a")
			if err != nil || len(listed) != 1 || listed[0].Metadata.Labels["project"] != "analysis" || listed[0].Metadata.Annotations["description"] != "worker" {
				t.Fatalf("list=%+v error=%v", listed, err)
			}
			listed[0].Metadata.Labels["project"] = "changed"
			got, err := m.GetTemplate(t.Context(), "team-a", "python-agent", result.ID)
			if err != nil || got.Metadata.Labels["project"] != "analysis" {
				t.Fatalf("get=%+v error=%v", got, err)
			}
		})
	}
	s := &templateBusinessStore{}
	_, err := templateBusinessManager(s).CreateTemplate(t.Context(), "team-a", "python-agent", templateconfig.Definition{})
	if managererrors.GetErrCode(err) != managererrors.ErrorBadRequest || s.createCalls != 0 {
		t.Fatalf("validation order: %v calls=%d", err, s.createCalls)
	}
}
func TestTemplateManagerLatestAndBoundSelection(t *testing.T) {
	now := time.Now()
	s := &templateBusinessStore{claim: &infra.TemplateClaimObservation{Identity: infra.TemplateClaimIdentity{Namespace: "team-a", Name: "python-agent", UID: "claim-uid"}}, versions: []infra.TemplateVersionObservation{
		{ID: "a7f2k9m4p1q8r3s6t0v5", CreatedAt: now.Add(-time.Second), Phase: "Ready"},
		{ID: "b7f2k9m4p1q8r3s6t0v5", CreatedAt: now, Phase: "Failed", Message: "Invalid definition."},
	}}
	m := templateBusinessManager(s)
	latest, err := m.GetTemplate(t.Context(), "team-a", "python-agent", "")
	if err != nil || latest.Status != "Failed" || latest.ID != s.versions[1].ID {
		t.Fatalf("latest=%+v %v", latest, err)
	}
	bound, err := m.GetTemplate(t.Context(), "team-a", "python-agent", s.versions[0].ID)
	if err != nil || bound.Status != "Ready" {
		t.Fatalf("bound=%+v %v", bound, err)
	}
	s.claim.Deleting = true
	deleted, err := m.GetTemplate(t.Context(), "team-a", "python-agent", s.versions[0].ID)
	if err != nil || deleted.Status != "Deleting" || deleted.Message != "" {
		t.Fatalf("summary=%+v %v", deleted, err)
	}
}
func TestTemplateManagerDeleteReferenceAndCancellation(t *testing.T) {
	for _, test := range []struct {
		name                           string
		absent, external, done, cancel bool
		want                           managererrors.ErrorCode
		begin                          bool
	}{
		{name: "external refuses before write", external: true, want: managererrors.ErrorConflict},
		{name: "completed", done: true, begin: true},
		{name: "timeout only ends wait", cancel: true, want: managererrors.ErrorUnavailable, begin: true},
		// The Claim is released last, so its live absence is the completion evidence.
		{name: "absent claim is already deleted", absent: true, external: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			s := &templateBusinessStore{claim: &infra.TemplateClaimObservation{Identity: infra.TemplateClaimIdentity{Namespace: "team-a", Name: "python-agent", UID: "fixed-uid"}}, external: test.external, done: test.done}
			if test.absent {
				s.claim = nil
			}
			m := templateBusinessManager(s)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if test.cancel {
				cancel()
			}
			err := m.DeleteTemplate(ctx, "team-a", "python-agent")
			if test.want == "" && err != nil || test.want != "" && managererrors.GetErrCode(err) != test.want {
				t.Fatalf("error=%v", err)
			}
			if (s.deleteCalls > 0) != test.begin {
				t.Fatalf("writes=%d", s.deleteCalls)
			}
			if test.absent && len(s.pinned) != 0 {
				t.Fatalf("absent claim must not start cleanup waits: %v", s.pinned)
			}
			for _, pinned := range s.pinned {
				if pinned.UID != "fixed-uid" {
					t.Fatal("request followed replacement")
				}
			}
			if test.cancel && !errors.Is(err, context.Canceled) {
				t.Fatal("lost cancellation cause")
			}
		})
	}
}
func TestTemplateIDRandomAllocation(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 64; i++ {
		id, err := allocateTemplateID()
		if err != nil || len(id) != 20 || seen[id] {
			t.Fatalf("id=%q error=%v", id, err)
		}
		seen[id] = true
		for _, c := range id {
			if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9') {
				t.Fatalf("invalid ID %q", id)
			}
		}
	}
}

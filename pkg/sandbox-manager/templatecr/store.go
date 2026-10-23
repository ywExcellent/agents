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

// Package templatecr stores logical Template coordination records in Kubernetes.
package templatecr

import (
	"context"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
)

// Store uses informer-backed reads by default and reserves live reads for
// resolving write conflicts where stale observations cannot decide the result.
type Store struct {
	client    client.Client
	apiReader func() client.Reader
}

// NewStore creates a logical Template store.
func NewStore(c client.Client, apiReader func() client.Reader) *Store {
	return &Store{client: c, apiReader: apiReader}
}

// Get returns the informer-backed view of a logical Template coordination CR.
func (s *Store) Get(ctx context.Context, namespace, name string) (*agentsv1alpha1.SandboxTemplate, error) {
	return get(ctx, s.client, namespace, name)
}

// GetFresh reads a logical Template directly from the API server. Callers use
// it only after a write conflict proves that the informer view may be stale.
func (s *Store) GetFresh(ctx context.Context, namespace, name string) (*agentsv1alpha1.SandboxTemplate, error) {
	return get(ctx, s.apiReader(), namespace, name)
}

func get(ctx context.Context, reader client.Reader, namespace, name string) (*agentsv1alpha1.SandboxTemplate, error) {
	template := &agentsv1alpha1.SandboxTemplate{}
	if err := reader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, template); err != nil {
		return nil, err
	}
	return template, nil
}

// Create persists a logical Template coordination CR.
func (s *Store) Create(ctx context.Context, template *agentsv1alpha1.SandboxTemplate) error {
	return s.client.Create(ctx, template)
}

// Update atomically replaces a logical Template using its resourceVersion.
func (s *Store) Update(ctx context.Context, template *agentsv1alpha1.SandboxTemplate) error {
	return s.client.Update(ctx, template)
}

// Delete removes the bound CR only when its UID and observed resourceVersion still match.
func (s *Store) Delete(ctx context.Context, namespace, name string, uid types.UID, resourceVersion string) error {
	template := &agentsv1alpha1.SandboxTemplate{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name}}
	preconditions := &metav1.Preconditions{UID: &uid}
	if resourceVersion != "" {
		preconditions.ResourceVersion = &resourceVersion
	}
	return s.client.Delete(ctx, template, &client.DeleteOptions{Preconditions: preconditions})
}

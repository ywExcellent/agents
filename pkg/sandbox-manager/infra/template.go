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
package infra

import (
	"context"
	"errors"
	"time"

	"github.com/openkruise/agents/pkg/templateconfig"
)

var (
	ErrTemplateMissing         = errors.New("template object missing")
	ErrTemplateExists          = errors.New("template object already exists")
	ErrTemplateViewUnavailable = errors.New("template control view unavailable")
	ErrTemplateScope           = errors.New("template namespace outside supported scope")
	ErrTemplateIdentityChanged = errors.New("template object identity changed")
	ErrTemplateInvalid         = errors.New("template persistence rejected the definition")
)

// TemplateClaimIdentity pins one incarnation of a logical name. UID is internal.
type TemplateClaimIdentity struct{ Namespace, Name, UID string }
type TemplateClaimObservation struct {
	Identity TemplateClaimIdentity
	Deleting bool
}

// TemplateVersionObservation contains only neutral definition observations.
type TemplateVersionObservation struct {
	ID, UID                           string
	CreatedAt                         time.Time
	Generation, ObservedGeneration    int64
	Phase                             string
	Deleting                          bool
	DefinitionReady, DefinitionFailed bool
	Message                           string
}

// TemplateStore is optional Kubernetes template capability, not an extension
// every Sandbox infrastructure backend is required to implement.
type TemplateStore interface {
	Run(context.Context) error
	Stop(context.Context)
	CreateClaim(context.Context, string, string, string) (TemplateClaimIdentity, error)
	CreateVersion(context.Context, TemplateClaimIdentity, string, templateconfig.Definition) error
	GetClaim(context.Context, string, string) (*TemplateClaimObservation, error)
	ListClaims(context.Context, string) ([]TemplateClaimObservation, error)
	ListVersions(context.Context, TemplateClaimIdentity) ([]TemplateVersionObservation, error)
	GetVersion(context.Context, TemplateClaimIdentity, string) (*TemplateVersionObservation, error)
	HasExternalReferences(context.Context, TemplateClaimIdentity) (bool, error)
	BeginDelete(context.Context, TemplateClaimIdentity) error
	Deleted(context.Context, TemplateClaimIdentity) (bool, error)
}
type TemplateStoreProvider interface{ GetTemplateStore() TemplateStore }

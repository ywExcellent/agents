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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	infracache "github.com/openkruise/agents/pkg/cache"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
)

type templateNameConflictCache struct {
	infracache.Provider
	client client.Client
}

func (c templateNameConflictCache) GetClient() client.Client {
	return c.client
}

func TestCheckLogicalTemplateNameConflict(t *testing.T) {
	tests := []struct {
		name      string
		object    client.Object
		want      infra.LogicalTemplateNameConflict
		lookupErr error
	}{
		{name: "no conflict", want: infra.LogicalTemplateNameConflictNone},
		{
			name:   "SandboxSet conflict",
			object: &agentsv1alpha1.SandboxSet{ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "template"}},
			want:   infra.LogicalTemplateNameConflictSandboxSet,
		},
		{
			name:   "Checkpoint conflict",
			object: &agentsv1alpha1.Checkpoint{ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "template"}},
			want:   infra.LogicalTemplateNameConflictCheckpoint,
		},
		{name: "lookup failure is preserved", lookupErr: errors.New("cache unavailable")},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			scheme := runtime.NewScheme()
			require.NoError(t, agentsv1alpha1.AddToScheme(scheme))
			builder := fake.NewClientBuilder().WithScheme(scheme)
			if tt.object != nil {
				builder = builder.WithObjects(tt.object)
			}
			base := builder.Build()
			var c client.Client = base
			if tt.lookupErr != nil {
				c = interceptor.NewClient(base, interceptor.Funcs{
					Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
						return tt.lookupErr
					},
				})
			}
			backend := &Infra{Cache: templateNameConflictCache{client: c}}

			got, err := backend.CheckLogicalTemplateNameConflict(t.Context(), infra.CheckLogicalTemplateNameConflictOptions{
				Namespace: "team-a",
				Name:      "template",
			})
			if tt.lookupErr != nil {
				require.ErrorIs(t, err, tt.lookupErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

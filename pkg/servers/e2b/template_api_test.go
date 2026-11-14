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
package e2b

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/openkruise/agents/pkg/servers/e2b/models"
	templAPI "github.com/openkruise/agents/pkg/servers/template"
)

func TestCustomTemplateAuthenticationUsesTeamNameAndSharedStore(t *testing.T) {
	storage := &lookupKeyStorage{byKey: map[string]*models.CreatedTeamAPIKey{
		"team":  {Team: &models.Team{ID: uuid.New(), Name: "team-a"}},
		"admin": {Team: models.AdminTeam()}, "legacy": {},
	}}
	sc := &Controller{keys: storage}
	for _, test := range []struct {
		key  string
		want error
	}{
		{key: "team"}, {key: "admin", want: templAPI.ErrForbidden}, {key: "legacy", want: templAPI.ErrForbidden}, {key: "invalid", want: templAPI.ErrUnauthenticated},
	} {
		t.Run(test.key, func(t *testing.T) {
			name, err := sc.authorizeTemplate(context.Background(), test.key)
			if !errors.Is(err, test.want) {
				t.Fatalf("error=%v", err)
			}
			if err == nil && name != "team-a" {
				t.Fatalf("scope=%q", name)
			}
		})
	}
	// Mutating the existing storage models revocation/reload; the new entrypoint
	// does not capture or duplicate a stale key map.
	delete(storage.byKey, "team")
	if _, err := sc.authorizeTemplate(t.Context(), "team"); !errors.Is(err, templAPI.ErrUnauthenticated) {
		t.Fatal("revocation not observed")
	}
}
func TestCustomTemplatePortRejectsNoAuthenticationAndCollisions(t *testing.T) {
	for _, port := range []int{-1, 65536, 8080, 9001, 9002, 7946} {
		sc := NewController(ControllerOptions{Port: 8080, CustomTemplatePort: port})
		if err := sc.Init(t.Context()); err == nil {
			t.Fatalf("port=%d unexpectedly initialized", port)
		}
	}
}

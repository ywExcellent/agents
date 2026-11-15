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

	"github.com/openkruise/agents/pkg/servers/e2b/keys"
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
	// With a key config present the collision branch runs; without one the
	// auth requirement fails first, which the "no authentication" case pins.
	authed := func(port int) ControllerOptions {
		return ControllerOptions{Port: 8080, CustomTemplatePort: port, KeyConfig: &keys.Config{Mode: keys.StorageModeSecret}}
	}
	const collision = "custom template listener conflicts with an existing listener"
	tests := []struct {
		name string
		opts ControllerOptions
		want string
	}{
		{name: "no authentication", opts: ControllerOptions{Port: 8080, CustomTemplatePort: 9001}, want: "custom template API requires team authentication"},
		{name: "below range", opts: authed(-1), want: "invalid custom template port"},
		{name: "above range", opts: authed(65536), want: "invalid custom template port"},
		{name: "legacy listener", opts: authed(8080), want: collision},
		{name: "route refresh listener", opts: authed(7789), want: collision},
		{name: "ext proc listener", opts: authed(9002), want: collision},
		{name: "memberlist listener", opts: authed(7946), want: collision},
		{name: "dedicated metrics listener", opts: ControllerOptions{Port: 8080, MetricsPort: 9001, CustomTemplatePort: 9001, KeyConfig: &keys.Config{Mode: keys.StorageModeSecret}}, want: collision},
		{name: "disabled passes the listener block", opts: authed(0), want: "ConfigSource is required"},
		{name: "free port passes the listener block", opts: authed(9001), want: "ConfigSource is required"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := NewController(test.opts).Init(t.Context())
			if err == nil || err.Error() != test.want {
				t.Fatalf("error=%v want %q", err, test.want)
			}
		})
	}
}

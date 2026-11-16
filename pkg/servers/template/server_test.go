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

package template

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	sandboxmanager "github.com/openkruise/agents/pkg/sandbox-manager"
	managererrors "github.com/openkruise/agents/pkg/sandbox-manager/errors"
	"github.com/openkruise/agents/pkg/templateconfig"
)

// These transport tests need only the four Manager operations, not a running
// Sandbox runtime, Kubernetes cluster, quota backend, or authentication worker.
type protocolTestService struct {
	namespace  string
	calls      int
	err        error
	summary    sandboxmanager.TemplateSummary
	definition templateconfig.Definition
}

func (s *protocolTestService) CreateTemplate(_ context.Context, ns, name string, d templateconfig.Definition) (sandboxmanager.TemplateSummary, error) {
	s.namespace = ns
	s.calls++
	s.definition = d
	if s.err != nil {
		return sandboxmanager.TemplateSummary{}, s.err
	}
	return sandboxmanager.TemplateSummary{Name: name, ID: "a7f2k9m4p1q8r3s6t0v5", Status: "Pending", Metadata: d.Metadata.Clone()}, nil
}
func (s *protocolTestService) GetTemplate(_ context.Context, ns, _, _ string) (sandboxmanager.TemplateSummary, error) {
	s.namespace = ns
	s.calls++
	return s.summary, s.err
}
func (s *protocolTestService) ListTemplates(_ context.Context, ns string) ([]sandboxmanager.TemplateSummary, error) {
	s.namespace = ns
	s.calls++
	return []sandboxmanager.TemplateSummary{}, s.err
}
func (s *protocolTestService) DeleteTemplate(_ context.Context, ns, _ string) error {
	s.namespace = ns
	s.calls++
	return s.err
}
func testAuthorize(_ context.Context, key string) (string, error) {
	switch key {
	case "team":
		return "team-a", nil
	case "admin", "legacy":
		return "", ErrForbidden
	default:
		return "", ErrUnauthenticated
	}
}
func TestTemplateHTTPContract(t *testing.T) {
	for _, test := range []struct {
		name, method, path, key, body string
		code                          int
		domain                        managererrors.ErrorCode
		called                        bool
	}{
		{name: "create", method: "POST", path: "/templates", key: "team", body: `{"name":"python-agent","spec":{"containers":[{"name":"main","image":"python:3.12"}]}}`, code: 201, called: true},
		{name: "list", method: "GET", path: "/templates", key: "team", code: 200, called: true},
		{name: "empty claim", method: "GET", path: "/templates/python-agent", key: "team", code: 200, called: true},
		{name: "bound get", method: "GET", path: "/templates/python-agent?templateId=a7f2k9m4p1q8r3s6t0v5", key: "team", code: 200, called: true},
		{name: "delete", method: "DELETE", path: "/templates/python-agent", key: "team", code: 204, called: true},
		{name: "admin", method: "GET", path: "/templates", key: "admin", code: 403},
		{name: "legacy", method: "GET", path: "/templates", key: "legacy", code: 403},
		{name: "missing key", method: "POST", path: "/templates", code: 401},
		{name: "invalid key", method: "GET", path: "/templates", key: "wrong", code: 401},
		{name: "unknown list filter", method: "GET", path: "/templates?name=a", key: "team", code: 400},
		{name: "delete condition not supported", method: "DELETE", path: "/templates/a?templateId=a", key: "team", code: 400},
		{name: "duplicate query", method: "GET", path: "/templates/a?templateId=a&templateId=b", key: "team", code: 400},
		{name: "invalid logical name", method: "GET", path: "/templates/UPPER", key: "team", code: 400},
		{name: "invalid bound id", method: "GET", path: "/templates/a?templateId=short", key: "team", code: 400},
		{name: "empty explicit id", method: "GET", path: "/templates/a?templateId=", key: "team", code: 400},
		{name: "old prefix not routed", method: "GET", path: "/kruise/api/templates", key: "team", code: 404},
		{name: "builds not routed", method: "POST", path: "/templates/a/builds", key: "team", code: 400},
		{name: "update not supported", method: "PATCH", path: "/templates/a", key: "team", code: 405},
		{name: "missing spec", method: "POST", path: "/templates", key: "team", body: `{"name":"a"}`, code: 400},
		{name: "not found", method: "GET", path: "/templates/a", key: "team", code: 404, domain: managererrors.ErrorNotFound, called: true},
		{name: "referenced", method: "DELETE", path: "/templates/a", key: "team", code: 409, domain: managererrors.ErrorConflict, called: true},
		{name: "cleanup pending", method: "DELETE", path: "/templates/a", key: "team", code: 503, domain: managererrors.ErrorUnavailable, called: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			service := &protocolTestService{summary: sandboxmanager.TemplateSummary{Name: "python-agent", Status: "Unknown"}}
			if test.domain != "" {
				service.err = managererrors.NewError(test.domain, "Controlled error.")
			}
			handler := NewServer(":0", service, testAuthorize)
			req := httptest.NewRequest(test.method, test.path, strings.NewReader(test.body))
			req.Header.Set("X-API-Key", test.key)
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, req)
			if recorder.Code != test.code {
				t.Fatalf("code=%d body=%s", recorder.Code, recorder.Body.String())
			}
			if (service.calls > 0) != test.called {
				t.Fatalf("manager calls=%d", service.calls)
			}
			if test.called && service.namespace != "team-a" {
				t.Fatal("team name not forwarded as scope")
			}
			if recorder.Code >= 400 {
				var e errorResponse
				if err := json.Unmarshal(recorder.Body.Bytes(), &e); err != nil {
					t.Fatal(err)
				}
				if e.Code != test.code || e.RequestID == "" {
					t.Fatalf("error=%+v", e)
				}
			}
			if test.name == "create" && recorder.Header().Get("Location") != "/templates/python-agent" {
				t.Fatal("missing Location")
			}
			if test.name == "empty claim" && !strings.Contains(recorder.Body.String(), `"templateId":null`) {
				t.Fatal("empty ID must be null")
			}
			if test.name == "list" && strings.TrimSpace(recorder.Body.String()) != "[]" {
				t.Fatalf("list=%s", recorder.Body.String())
			}
			if test.code == 204 && recorder.Body.Len() != 0 {
				t.Fatal("204 must have no body")
			}
		})
	}
}
func TestTemplateStrictInputPresence(t *testing.T) {
	prefix := `{"name":"a","spec":{"containers":[{"name":"main","image":"python:3.12"}]}`
	for _, test := range []struct {
		name, raw string
		ok        bool
	}{
		{name: "minimal", raw: prefix + `}`, ok: true},
		{name: "null root metadata", raw: prefix + `,"metadata":null}`, ok: true},
		{name: "empty metadata", raw: prefix + `,"metadata":{}}`, ok: true},
		{name: "business metadata", raw: prefix + `,"metadata":{"labels":{"app.kubernetes.io/name":"worker"},"annotations":{"description":"free text"}}}`, ok: true},
		{name: "annotation budget boundary", raw: prefix + `,"metadata":{"annotations":{"x":"` + strings.Repeat("a", 64*1024-1) + `"}}}`, ok: true},
		{name: "annotation budget exceeded", raw: prefix + `,"metadata":{"annotations":{"x":"` + strings.Repeat("a", 64*1024) + `"}}}`},
		{name: "metadata name forbidden", raw: prefix + `,"metadata":{"name":"fake"}}`},
		{name: "metadata namespace forbidden", raw: prefix + `,"metadata":{"namespace":"other"}}`},
		{name: "metadata UID forbidden", raw: prefix + `,"metadata":{"uid":"fake"}}`},
		{name: "metadata owner forbidden", raw: prefix + `,"metadata":{"ownerReferences":[]}}`},
		{name: "metadata finalizers forbidden", raw: prefix + `,"metadata":{"finalizers":[]}}`},
		{name: "metadata scalar", raw: prefix + `,"metadata":"fake"}`},
		{name: "metadata array", raw: prefix + `,"metadata":[]}`},
		{name: "metadata wrong case", raw: prefix + `,"metadata":{"Labels":{}}}`},
		{name: "duplicate metadata map", raw: prefix + `,"metadata":{"labels":{},"labels":{}}}`},
		{name: "duplicate metadata key", raw: prefix + `,"metadata":{"labels":{"app":"a","app":"b"}}}`},
		{name: "metadata null map", raw: prefix + `,"metadata":{"annotations":null}}`},
		{name: "metadata null value", raw: prefix + `,"metadata":{"labels":{"app":null}}}`},
		{name: "metadata number value", raw: prefix + `,"metadata":{"labels":{"app":1}}}`},
		{name: "metadata boolean value", raw: prefix + `,"metadata":{"annotations":{"app":true}}}`},
		{name: "metadata invalid key", raw: prefix + `,"metadata":{"labels":{"bad key":"v"}}}`},
		{name: "metadata invalid label value", raw: prefix + `,"metadata":{"labels":{"app":"free text"}}}`},
		{name: "metadata reserved label", raw: prefix + `,"metadata":{"labels":{"agents.kruise.io/sandbox-id":"fake"}}}`},
		{name: "metadata reserved annotation", raw: prefix + `,"metadata":{"annotations":{"sandbox.agents.kruise.io/hash-immutable-part":"fake"}}}`},
		{name: "null options", raw: prefix + `,"options":null}`, ok: true},
		{name: "default pool", raw: prefix + `,"options":{"warmPool":{}}}`, ok: true},
		{name: "disabled", raw: prefix + `,"options":{"warmPool":{"enabled":false}}}`, ok: true},
		{name: "disabled explicit zero", raw: prefix + `,"options":{"warmPool":{"enabled":false,"size":0}}}`},
		{name: "disabled null auto", raw: prefix + `,"options":{"warmPool":{"enabled":false,"autoscaler":null}}}`},
		{name: "null integer", raw: prefix + `,"options":{"warmPool":{"size":null}}}`},
		{name: "float integer", raw: prefix + `,"options":{"warmPool":{"size":1.2}}}`},
		{name: "bool integer", raw: prefix + `,"options":{"warmPool":{"size":true}}}`},
		{name: "overflow", raw: prefix + `,"options":{"warmPool":{"size":2147483648}}}`},
		{name: "legacy command", raw: prefix + `,"options":{"steps":[]}}`},
		{name: "unknown field", raw: prefix + `,"namespace":"team-a"}`},
		{name: "wrong case", raw: `{"Name":"a","spec":{"containers":[]}}`},
		{name: "duplicate field", raw: `{"name":"a","name":"b","spec":{"containers":[]}}`},
		{name: "multiple JSON values", raw: prefix + `} {}`},
		{name: "PVC labels even empty", raw: `{"name":"a","spec":{"containers":[{"name":"main","image":"python:3.12"}],"volumeClaimTemplates":[{"metadata":{"name":"data","labels":{}},"spec":{}}]}}`},
		{name: "PVC status", raw: `{"name":"a","spec":{"containers":[],"volumeClaimTemplates":[{"metadata":{"name":"data"},"spec":{},"status":null}]}}`},
		{name: "upgrade hook null", raw: `{"name":"a","spec":{"containers":[],"lifecycle":{"preUpgrade":null}}}`},
		{name: "native standard fields", raw: `{"name":"a","spec":{"containers":[{"name":"main","image":"python:3.12","resources":{"limits":{"cpu":"1","memory":"1Gi"}}}],"nodeSelector":{"tier":"sandbox"}}}`, ok: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := decodeCreate(strings.NewReader(test.raw))
			if (err == nil) != test.ok {
				t.Fatalf("error=%v", err)
			}
		})
	}
}
func TestCustomTemplateListenerRootRoutingAndShutdown(t *testing.T) {
	service := &protocolTestService{}
	s := NewServer("127.0.0.1:0", service, testAuthorize)
	addresses := make(chan string, 1)
	s.http.BaseContext = func(l net.Listener) context.Context { addresses <- l.Addr().String(); return t.Context() }
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := s.Shutdown(ctx); err != nil {
			t.Error(err)
		}
	})
	var address string
	select {
	case address = <-addresses:
	case <-time.After(time.Second):
		t.Fatal("listener did not start")
	}
	cl := &http.Client{Timeout: time.Second, Transport: &http.Transport{Proxy: nil}}
	t.Cleanup(cl.CloseIdleConnections)
	for _, test := range []struct {
		path string
		code int
	}{{"/templates", 200}, {"/kruise/api/templates", 404}, {"/agents.kruise.io/v1alpha1/templates", 404}} {
		request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+address+test.path, nil)
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("X-API-Key", "team")
		response, err := cl.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.Copy(io.Discard, response.Body); err != nil {
			t.Fatal(err)
		}
		if err := response.Body.Close(); err != nil {
			t.Fatal(err)
		}
		if response.StatusCode != test.code {
			t.Fatalf("path=%s status=%d", test.path, response.StatusCode)
		}
	}
}

func TestTemplateMetadataHTTPProjection(t *testing.T) {
	for _, method := range []string{"POST", "GET"} {
		t.Run(method, func(t *testing.T) {
			service := &protocolTestService{summary: sandboxmanager.TemplateSummary{Name: "demo", ID: "abcdefghijklmnopqrst", Status: "Ready", Metadata: templateconfig.Metadata{Labels: map[string]string{"project": "analysis", "agents.kruise.io/id": "secret"}, Annotations: map[string]string{"description": "worker", "agents.kruise.io/template-allocation": "secret"}}}}
			path := "/templates/demo"
			if method == "POST" {
				path = "/templates"
			}
			request := httptest.NewRequest(method, path, strings.NewReader(`{"name":"demo","metadata":{"labels":{"project":"analysis"},"annotations":{"description":"worker"}},"spec":{"containers":[{"name":"main","image":"python:3.12"}]}}`))
			request.Header.Set("X-API-Key", "team")
			recorder := httptest.NewRecorder()
			NewServer(":0", service, testAuthorize).ServeHTTP(recorder, request)
			var result response
			if err := json.Unmarshal(recorder.Body.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			if result.Metadata == nil || len(result.Metadata.Labels) != 1 || len(result.Metadata.Annotations) != 1 || result.Metadata.Labels["project"] != "analysis" || result.Metadata.Annotations["description"] != "worker" {
				t.Fatalf("body=%s", recorder.Body.String())
			}
			if method == "POST" && (service.definition.Metadata.Labels["project"] != "analysis" || len(service.definition.TemplateSpec().Template.Labels) > 0) {
				t.Fatal("root metadata missing or injected into PodTemplate")
			}
		})
	}
	result := output(sandboxmanager.TemplateSummary{Name: "empty", Status: "Unknown"})
	if result.Metadata != nil {
		t.Fatal("empty metadata must be omitted for old clients")
	}
}

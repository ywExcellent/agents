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
	"errors"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/klog/v2"

	sandboxmanager "github.com/openkruise/agents/pkg/sandbox-manager"
	managererrors "github.com/openkruise/agents/pkg/sandbox-manager/errors"
	"github.com/openkruise/agents/pkg/templateconfig"
	"github.com/openkruise/agents/pkg/templatevalidation"
)

const DefaultPort = 8081

var (
	ErrUnauthenticated = errors.New("invalid template API credentials")
	ErrForbidden       = errors.New("a team scoped API key is required")
)

// Authorize uses the owning API authentication runtime on every request, so
// secret reload and revocation are shared with the original listener.
type Authorize func(context.Context, string) (string, error)
type response struct {
	Name       string  `json:"name"`
	TemplateID *string `json:"templateId"`
	Status     string  `json:"status"`
	Message    string  `json:"message,omitempty"`
}
type errorResponse struct {
	Code      int    `json:"code"`
	Message   string `json:"message"`
	RequestID string `json:"request_id"`
}

// Service is the current Manager use-case surface used by this protocol.
// It keeps transport tests independent of the full Sandbox runtime.
type Service interface {
	CreateTemplate(context.Context, string, string, templateconfig.Definition) (sandboxmanager.TemplateSummary, error)
	GetTemplate(context.Context, string, string, string) (sandboxmanager.TemplateSummary, error)
	ListTemplates(context.Context, string) ([]sandboxmanager.TemplateSummary, error)
	DeleteTemplate(context.Context, string, string) error
}
type Server struct {
	manager   Service
	authorize Authorize
	http      *http.Server
}

func NewServer(address string, m Service, authorize Authorize) *Server {
	s := &Server{manager: m, authorize: authorize}
	s.http = &http.Server{Addr: address, Handler: s, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second}
	return s
}
func (s *Server) Start() error {
	listener, err := net.Listen("tcp", s.http.Addr)
	if err != nil {
		return err
	}
	go func() {
		if err := s.http.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) && !errors.Is(err, net.ErrClosed) {
			klog.ErrorS(err, "custom template HTTP listener stopped")
		}
	}()
	return nil
}
func (s *Server) Shutdown(ctx context.Context) error {
	if err := s.http.Shutdown(ctx); err != nil {
		return errors.Join(err, s.http.Close())
	}
	return nil
}
func (s *Server) Close() error { return s.http.Close() }
func output(summary sandboxmanager.TemplateSummary) response {
	result := response{Name: summary.Name, Status: summary.Status, Message: summary.Message}
	if summary.ID != "" {
		id := summary.ID
		result.TemplateID = &id
	}
	return result
}
func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		klog.ErrorS(err, "write template response")
	}
}
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	r = r.WithContext(ctx)
	requestID := uuid.NewString()
	fail := func(code int, message string) {
		writeJSON(w, code, errorResponse{Code: code, Message: message, RequestID: requestID})
	}
	key := r.Header.Get("X-API-Key")
	if key == "" {
		fail(401, "Invalid API Key")
		return
	}
	namespace, err := s.authorize(r.Context(), key)
	if err != nil {
		if errors.Is(err, ErrForbidden) {
			fail(403, "A team-scoped API key is required.")
		} else {
			fail(401, "Invalid API Key")
		}
		return
	}
	if namespace == "" {
		fail(403, "A team-scoped API key is required.")
		return
	}
	path := r.URL.Path
	if path != "/templates" && !strings.HasPrefix(path, "/templates/") {
		fail(404, "Route not found.")
		return
	}
	name := ""
	if path != "/templates" {
		name = strings.TrimPrefix(path, "/templates/")
		if name == "" || strings.Contains(name, "/") {
			fail(400, "Invalid template route.")
			return
		}
	}
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		fail(400, "Invalid query parameters.")
		return
	}
	id := ""
	for key, values := range query {
		if r.Method != http.MethodGet || name == "" || key != "templateId" || len(values) != 1 || values[0] == "" {
			fail(400, "Unsupported query parameter.")
			return
		}
		id = values[0]
	}
	if name != "" && len(templatevalidation.ValidateName(name, field.NewPath("name"))) > 0 || id != "" && len(templatevalidation.ValidateID(id, field.NewPath("templateId"))) > 0 {
		fail(400, "Invalid template name or ID.")
		return
	}

	mapError := func(err error) {
		code := 500
		switch managererrors.GetErrCode(err) {
		case managererrors.ErrorBadRequest:
			code = 400
		case managererrors.ErrorNotFound:
			code = 404
		case managererrors.ErrorConflict:
			code = 409
		case managererrors.ErrorNotAllowed:
			code = 403
		case managererrors.ErrorUnavailable:
			code = 503
		}
		var domain *managererrors.Error
		message := "Template operation failed."
		if errors.As(err, &domain) {
			message = domain.Message
		}
		fail(code, message)
	}
	switch {
	case r.Method == http.MethodPost && name == "":
		if content := r.Header.Get("Content-Type"); content != "" {
			mediaType, _, err := mime.ParseMediaType(content)
			if err != nil || mediaType != "application/json" {
				fail(400, "Expected application/json.")
				return
			}
		}
		in, err := decodeCreate(r.Body)
		if err != nil {
			fail(400, "Invalid template request.")
			return
		}
		if len(templatevalidation.ValidateName(in.Name, field.NewPath("name"))) > 0 {
			fail(400, "Invalid template name.")
			return
		}
		result, err := s.manager.CreateTemplate(ctx, namespace, in.Name, in.definition())
		if err != nil {
			mapError(err)
			return
		}
		w.Header().Set("Location", "/templates/"+url.PathEscape(in.Name))
		writeJSON(w, 201, output(result))
	case r.Method == http.MethodGet && name == "":
		summaries, err := s.manager.ListTemplates(ctx, namespace)
		if err != nil {
			mapError(err)
			return
		}
		result := make([]response, 0, len(summaries))
		for _, summary := range summaries {
			result = append(result, output(summary))
		}
		writeJSON(w, 200, result)
	case r.Method == http.MethodGet && name != "":
		summary, err := s.manager.GetTemplate(ctx, namespace, name, id)
		if err != nil {
			mapError(err)
			return
		}
		writeJSON(w, 200, output(summary))
	case r.Method == http.MethodDelete && name != "":
		if r.Header.Get("If-Match") != "" || r.Header.Get("If-None-Match") != "" {
			fail(400, "Conditional template deletion is not supported.")
			return
		}
		if err := s.manager.DeleteTemplate(ctx, namespace, name); err != nil {
			mapError(err)
			return
		}
		w.WriteHeader(204)
	default:
		if name == "" {
			w.Header().Set("Allow", "GET, POST")
		} else {
			w.Header().Set("Allow", "GET, DELETE")
		}
		fail(405, "Method not supported.")
	}
}

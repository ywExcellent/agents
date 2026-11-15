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
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	toolscache "k8s.io/client-go/tools/cache"
	ctrlcache "sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"

	api "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/templatecreation"
	"github.com/openkruise/agents/pkg/templateindex"
)

// templateKeyReader deliberately cannot List. Real reads are restricted to the
// three keyed correctness uses approved for Template API on 2026-11-13.
type templateKeyReader interface {
	Get(context.Context, client.ObjectKey, client.Object, ...client.GetOption) error
}

type templateView struct {
	cache      ctrlcache.Cache
	healthy    atomic.Bool
	closed     atomic.Bool
	watchError atomic.Int64
	mu         sync.Mutex
	cancel     context.CancelFunc
	done       chan struct{}
}

func NewTemplateStore(config *rest.Config, namespace string) (*templateStore, error) {
	scheme := runtime.NewScheme()
	if err := api.AddToScheme(scheme); err != nil {
		return nil, err
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		return nil, err
	}
	view := &templateView{}
	opts := ctrlcache.Options{Scheme: scheme, ReaderFailOnMissingInformer: true,
		DefaultWatchErrorHandler: func(_ context.Context, _ *toolscache.Reflector, err error) {
			if err != nil {
				view.watchError.Store(time.Now().UnixNano())
			}
		},
		ByObject: map[client.Object]ctrlcache.ByObject{}}
	if namespace != "" {
		opts.DefaultNamespaces = map[string]ctrlcache.Config{namespace: {}}
	}
	for _, o := range []client.Object{&api.SandboxTemplateClaim{}, &api.SandboxTemplate{}, &api.SandboxSet{}} {
		opts.ByObject[o] = ctrlcache.ByObject{}
	}
	c, err := ctrlcache.New(config, opts)
	if err != nil {
		return nil, fmt.Errorf("create template view: %w", err)
	}
	view.cache = c
	if err := templateindex.Register(context.Background(), c); err != nil {
		return nil, err
	}
	for o := range opts.ByObject {
		if _, err := c.GetInformer(context.Background(), o, ctrlcache.BlockUntilSynced(false)); err != nil {
			return nil, fmt.Errorf("initialize template informer: %w", err)
		}
	}
	writeConfig := rest.CopyConfig(config)
	writeConfig.Timeout = templatecreation.CreationRequestBudget
	previous := writeConfig.WrapTransport
	writeConfig.WrapTransport = func(rt http.RoundTripper) http.RoundTripper {
		if previous != nil {
			rt = previous(rt)
		}
		return templateRequestDeadlineTransport{next: rt}
	}
	writer, err := client.New(writeConfig, client.Options{Scheme: scheme})
	if err != nil {
		return nil, err
	}
	return &templateStore{cached: c, writer: writer, live: writer, namespace: namespace, view: view}, nil
}

func (v *templateView) Run(ctx context.Context) error {
	v.mu.Lock()
	if v.closed.Load() {
		v.mu.Unlock()
		return fmt.Errorf("template view already stopped")
	}
	if v.done != nil {
		v.mu.Unlock()
		return fmt.Errorf("template view already started")
	}
	runCtx, cancel := context.WithCancel(ctx)
	v.cancel = cancel
	v.done = make(chan struct{})
	done := v.done
	v.mu.Unlock()
	stopped := make(chan error, 1)
	go func() { defer close(done); err := v.cache.Start(runCtx); v.healthy.Store(false); stopped <- err }()
	syncCtx, syncCancel := context.WithTimeout(runCtx, 30*time.Second)
	defer syncCancel()
	if !v.cache.WaitForCacheSync(syncCtx) {
		cancel()
		return fmt.Errorf("template control cache initial synchronization failed")
	}
	v.healthy.Store(true)
	select {
	case err := <-stopped:
		v.healthy.Store(false)
		cancel()
		if err != nil {
			return fmt.Errorf("template cache stopped during startup: %w", err)
		}
		return fmt.Errorf("template cache stopped before readiness")
	default:
	}
	return nil
}
func (v *templateView) Stop(ctx context.Context) {
	v.mu.Lock()
	v.closed.Store(true)
	cancel, done := v.cancel, v.done
	v.mu.Unlock()
	v.healthy.Store(false)
	if cancel == nil {
		return
	}
	cancel()
	select {
	case <-done:
	case <-ctx.Done():
	}
}
func (v *templateView) Healthy() bool {
	if v.closed.Load() || !v.healthy.Load() {
		return false
	}
	last := v.watchError.Load()
	return last == 0 || time.Since(time.Unix(0, last)) >= 2*time.Minute
}

// compile-time assertion: all live access is Get-only.
var _ templateKeyReader = client.Reader(nil)

// templateRequestDeadlineTransport applies a server-side deadline to this
// store's Create calls as well as the caller context deadline. It does not
// mutate the shared REST config or affect informer watches/legacy consumers.
type templateRequestDeadlineTransport struct{ next http.RoundTripper }

func (t templateRequestDeadlineTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Method != http.MethodPost {
		return t.next.RoundTrip(req)
	}
	budget := templatecreation.CreationRequestBudget
	if deadline, ok := req.Context().Deadline(); ok {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return nil, context.DeadlineExceeded
		}
		if remaining < budget {
			budget = remaining
		}
	}
	cloned := req.Clone(req.Context())
	query := cloned.URL.Query()
	query.Set("timeout", budget.String())
	cloned.URL.RawQuery = query.Encode()
	return t.next.RoundTrip(cloned)
}

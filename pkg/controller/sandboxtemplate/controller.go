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
// Package sandboxtemplate reconciles Claim-owned definitions and companions.
// It is independent of the API, Manager and Sandbox Infra implementations.
package sandboxtemplate

import (
	"context"
	"encoding/json"
	"fmt"
	"k8s.io/apimachinery/pkg/types"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/recorder"

	api "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/discovery"
	"github.com/openkruise/agents/pkg/templatecreation"
	"github.com/openkruise/agents/pkg/templateindex"
	"github.com/openkruise/agents/pkg/templatevalidation"
)

// KeyReader limits approved live reads to known object keys; it cannot List.
type KeyReader interface {
	Get(context.Context, client.ObjectKey, client.Object, ...client.GetOption) error
}

type Reconciler struct {
	client.Client
	Reader   KeyReader
	Scheme   *runtime.Scheme
	Recorder recorder.EventRecorder
}

const retryInterval = 2 * time.Second

// +kubebuilder:rbac:groups=agents.kruise.io,resources=sandboxtemplateclaims;sandboxtemplates;sandboxsets;poolautoscalers,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=agents.kruise.io,resources=sandboxtemplateclaims/finalizers;sandboxtemplates/finalizers,verbs=update
// +kubebuilder:rbac:groups=agents.kruise.io,resources=sandboxtemplates/status,verbs=get;update;patch

func Add(m manager.Manager) error {
	if !discovery.DiscoverGVK(api.SandboxTemplateClaimControllerKind) {
		return nil
	}
	for _, entry := range []struct {
		o   client.Object
		key string
		f   client.IndexerFunc
	}{
		{&api.SandboxTemplate{}, templateindex.ClaimOwner, templateindex.ClaimOwnerUID},
		{&api.SandboxSet{}, templateindex.VersionOwner, templateindex.VersionOwnerUID},
		{&api.PoolAutoscaler{}, templateindex.VersionOwner, templateindex.VersionOwnerUID},
	} {
		if err := m.GetFieldIndexer().IndexField(context.Background(), entry.o, entry.key, entry.f); err != nil {
			return err
		}
	}
	r := &Reconciler{Client: m.GetClient(), Reader: m.GetAPIReader(), Scheme: m.GetScheme(), Recorder: m.GetEventRecorder("sandbox-template")}
	if err := ctrl.NewControllerManagedBy(m).Named("sandbox-template-version").For(&api.SandboxTemplate{}).
		Owns(&api.SandboxSet{}).Owns(&api.PoolAutoscaler{}).
		Watches(&api.SandboxTemplateClaim{}, handler.EnqueueRequestsFromMapFunc(r.versionsForClaim)).Complete(reconcile.Func(r.ReconcileVersion)); err != nil {
		return err
	}
	return ctrl.NewControllerManagedBy(m).Named("sandbox-template-claim").For(&api.SandboxTemplateClaim{}).Owns(&api.SandboxTemplate{}).Complete(reconcile.Func(r.ReconcileClaim))
}
func (r *Reconciler) versionsForClaim(ctx context.Context, o client.Object) []reconcile.Request {
	list := &api.SandboxTemplateList{}
	if err := r.List(ctx, list, client.InNamespace(o.GetNamespace()), client.MatchingFields{templateindex.ClaimOwner: string(o.GetUID())}); err != nil {
		log.FromContext(ctx).Error(err, "list template versions for claim event")
		return nil
	}
	out := []reconcile.Request{}
	for _, v := range list.Items {
		out = append(out, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&v)})
	}
	return out
}
func (r *Reconciler) ReconcileClaim(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	c := &api.SandboxTemplateClaim{}
	if err := r.Reader.Get(ctx, req.NamespacedName, c); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if c.DeletionTimestamp == nil {
		return ctrl.Result{}, nil
	}
	if !controllerutil.ContainsFinalizer(c, api.SandboxTemplateClaimCleanupFinalizer) {
		return ctrl.Result{}, nil
	}
	list := &api.SandboxTemplateList{}
	if err := r.List(ctx, list, client.InNamespace(c.Namespace), client.MatchingFields{templateindex.ClaimOwner: string(c.UID)}); err != nil {
		return ctrl.Result{}, err
	}
	versions := map[string]*api.SandboxTemplate{}
	for i := range list.Items {
		v := &list.Items[i]
		o := templateindex.ClaimOwnerReference(v)
		if o != nil && o.Name == c.Name && o.UID == c.UID {
			versions[v.Name] = v
		}
	}
	// This known key closes the multi-replica negative-list gap, without a live
	// List or a default ref. Ownership is still checked against actual Claim UID.
	if id := c.Annotations[templatecreation.AllocationAnnotation]; id != "" {
		v := &api.SandboxTemplate{}
		key := client.ObjectKey{Namespace: c.Namespace, Name: id}
		err := r.Get(ctx, key, v)
		cachedOwner := templateindex.ClaimOwnerReference(v)
		if apierrors.IsNotFound(err) || err == nil && (cachedOwner == nil || cachedOwner.UID != c.UID || cachedOwner.Name != c.Name) {
			err = r.Reader.Get(ctx, key, v)
		}
		if err != nil && !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
		if err == nil {
			o := templateindex.ClaimOwnerReference(v)
			if o != nil && o.Name == c.Name && o.UID == c.UID {
				versions[id] = v
			}
		}
	}
	for _, candidate := range versions {
		v := &api.SandboxTemplate{}
		err := r.Get(ctx, client.ObjectKeyFromObject(candidate), v)
		if apierrors.IsNotFound(err) {
			err = r.Reader.Get(ctx, client.ObjectKeyFromObject(candidate), v)
		}
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return ctrl.Result{}, err
		}
		owner := templateindex.ClaimOwnerReference(v)
		if v.UID != candidate.UID || owner == nil || owner.Name != c.Name || owner.UID != c.UID {
			continue
		}
		if v.DeletionTimestamp == nil {
			if err := r.Delete(ctx, v, client.Preconditions{UID: &v.UID, ResourceVersion: &v.ResourceVersion}, client.PropagationPolicy(metav1.DeletePropagationBackground)); err != nil && !apierrors.IsNotFound(err) {
				return ctrl.Result{}, err
			}
		}
		// A child must be truly gone (including its foreground GC finalizer).
		return ctrl.Result{RequeueAfter: retryInterval}, nil
	}
	// If the creator crashed or an HTTP Create failed ambiguously, do not
	// release an empty name while its bounded version write can still finish.
	// This uses only durable metadata and survives controller/process restart.
	if c.Annotations[templatecreation.CreationCompleteAnnotation] != "true" && !c.CreationTimestamp.IsZero() {
		remaining := time.Until(c.CreationTimestamp.Add(templatecreation.EmptyCreationGrace))
		if remaining > 0 {
			return ctrl.Result{RequeueAfter: remaining}, nil
		}
	}
	before := c.DeepCopy()
	controllerutil.RemoveFinalizer(c, api.SandboxTemplateClaimCleanupFinalizer)
	return ctrl.Result{}, r.Patch(ctx, c, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{}))
}
func (r *Reconciler) ReconcileVersion(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	v := &api.SandboxTemplate{}
	if err := r.Get(ctx, req.NamespacedName, v); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	o := templateindex.ClaimOwnerReference(v)
	if o == nil {
		return ctrl.Result{}, nil
	}
	c := &api.SandboxTemplateClaim{}
	err := r.Reader.Get(ctx, client.ObjectKey{Namespace: v.Namespace, Name: o.Name}, c)
	if err != nil && !apierrors.IsNotFound(err) {
		return ctrl.Result{}, err
	}
	deleting := v.DeletionTimestamp != nil || apierrors.IsNotFound(err) || c.UID != o.UID || c.DeletionTimestamp != nil
	if deleting {
		if v.DeletionTimestamp == nil {
			if err := r.Delete(ctx, v, client.Preconditions{UID: &v.UID, ResourceVersion: &v.ResourceVersion}, client.PropagationPolicy(metav1.DeletePropagationBackground)); err != nil {
				return ctrl.Result{}, client.IgnoreNotFound(err)
			}
			return ctrl.Result{RequeueAfter: retryInterval}, nil
		}
		return r.cleanupVersion(ctx, v)
	}
	definition := meta.FindStatusCondition(v.Status.Conditions, templatecreation.DefinitionCondition)
	ready := v.Status.Phase == api.SandboxTemplatePhaseReady && v.Status.ObservedGeneration == v.Generation && definition != nil && definition.ObservedGeneration == v.Generation && definition.Status == metav1.ConditionTrue
	failed := v.Status.Phase == api.SandboxTemplatePhaseFailed && v.Status.ObservedGeneration == v.Generation && definition != nil && definition.ObservedGeneration == v.Generation && definition.Status == metav1.ConditionFalse && definition.Reason == "InvalidDefinition"
	if !ready && !failed {
		old := v.DeepCopy()
		_, errs := templatevalidation.NormalizeCustomSpec(&v.Spec, field.NewPath("spec"))
		phase, condition, reason, message := api.SandboxTemplatePhaseReady, metav1.ConditionTrue, "ValidDefinition", "Template definition validation passed."
		if len(errs) > 0 {
			phase, condition, reason, message = api.SandboxTemplatePhaseFailed, metav1.ConditionFalse, "InvalidDefinition", "Template definition validation failed."
		}
		v.Status.Phase = phase
		v.Status.ObservedGeneration = v.Generation
		meta.SetStatusCondition(&v.Status.Conditions, metav1.Condition{Type: templatecreation.DefinitionCondition, Status: condition, ObservedGeneration: v.Generation, Reason: reason, Message: message})
		if err := r.Status().Patch(ctx, v, client.MergeFromWithOptions(old, client.MergeFromWithOptimisticLock{})); err != nil {
			return ctrl.Result{}, err
		}
	}
	if v.Status.Phase != api.SandboxTemplatePhaseReady {
		return ctrl.Result{}, nil
	}
	if err := r.ensurePool(ctx, v); err != nil {
		log.FromContext(ctx).Error(err, "reconcile template companion", "template", req.NamespacedName)
		if r.Recorder != nil {
			r.Recorder.Eventf(v, nil, corev1.EventTypeWarning, "CompanionReconcileFailed", "Reconcile", "Companion pool reconciliation failed; inspect controller logs.")
		}
		return ctrl.Result{RequeueAfter: retryInterval}, err
	}
	return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
}
func ownedByVersion(o client.Object, v *api.SandboxTemplate) bool {
	ref := metav1.GetControllerOfNoCopy(o)
	return o.GetNamespace() == v.Namespace && ref != nil && ref.APIVersion == api.GroupVersion.String() && ref.Kind == "SandboxTemplate" && ref.Name == v.Name && ref.UID == v.UID
}
func (r *Reconciler) ensurePool(ctx context.Context, v *api.SandboxTemplate) error {
	if v.Spec.BuildOptions == nil || v.Spec.BuildOptions.WarmPool == nil {
		return nil
	}
	warm := v.Spec.BuildOptions.WarmPool
	if !ptr.Deref(warm.Enabled, true) {
		return nil
	}
	p := &api.SandboxSet{}
	err := r.Get(ctx, client.ObjectKey{Namespace: v.Namespace, Name: v.Name}, p)
	if apierrors.IsNotFound(err) {
		p = &api.SandboxSet{ObjectMeta: metav1.ObjectMeta{Namespace: v.Namespace, Name: v.Name}, Spec: api.SandboxSetSpec{Replicas: ptr.Deref(warm.Size, 0)}}
		p.Spec.TemplateRef = &api.SandboxTemplateRef{Name: v.Name, Kind: ptr.To("SandboxTemplate"), APIVersion: ptr.To(api.GroupVersion.String())}
		if err := controllerutil.SetControllerReference(v, p, r.Scheme); err != nil {
			return err
		}
		return r.Create(ctx, p)
	}
	if err != nil {
		return err
	}
	if !ownedByVersion(p, v) {
		return fmt.Errorf("companion name occupied by an external sandboxset")
	}
	if p.DeletionTimestamp != nil {
		return fmt.Errorf("companion pool is being deleted")
	}
	expected := &api.SandboxTemplateRef{Name: v.Name, Kind: ptr.To("SandboxTemplate"), APIVersion: ptr.To(api.GroupVersion.String())}
	if p.Spec.Template != nil || !equality.Semantic.DeepEqual(p.Spec.TemplateRef, expected) {
		return fmt.Errorf("managed pool definition reference differs from its owner")
	}
	if warm.Autoscaler == nil && p.Spec.Replicas != ptr.Deref(warm.Size, 0) {
		before := p.DeepCopy()
		p.Spec.Replicas = ptr.Deref(warm.Size, 0)
		return r.Patch(ctx, p, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{}))
	}
	return nil
}

// deleteKnownCompanion confirms negative owned-target observations live. Its actual
// controller UID, not its name, is the deletion authority.
func (r *Reconciler) deleteKnownCompanion(ctx context.Context, v *api.SandboxTemplate, o client.Object, foreground bool) (bool, error) {
	name := o.GetName()
	if name == "" {
		name = v.Name
	}
	key := client.ObjectKey{Namespace: v.Namespace, Name: name}
	err := r.Get(ctx, key, o)
	// Confirm a negative observation of the known owned cleanup target, not
	// every positive cache hit. No other live reads or live Lists are used.
	if apierrors.IsNotFound(err) || err == nil && !ownedByVersion(o, v) {
		err = r.Reader.Get(ctx, key, o)
	}
	if apierrors.IsNotFound(err) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	if !ownedByVersion(o, v) {
		return true, nil
	}
	if o.GetDeletionTimestamp() == nil {
		policy := metav1.DeletePropagationBackground
		if foreground {
			policy = metav1.DeletePropagationForeground
		}
		if err := r.Delete(ctx, o, client.Preconditions{UID: ptr.To(o.GetUID()), ResourceVersion: ptr.To(o.GetResourceVersion())}, client.PropagationPolicy(policy)); err != nil && !apierrors.IsNotFound(err) {
			return false, err
		}
	}
	return false, nil
}
func (r *Reconciler) deleteOwnedCompanions(ctx context.Context, v *api.SandboxTemplate, autoscalers bool) (bool, error) {
	var list client.ObjectList
	var expected client.Object
	if autoscalers {
		list = &api.PoolAutoscalerList{}
		expected = &api.PoolAutoscaler{}
	} else {
		list = &api.SandboxSetList{}
		expected = &api.SandboxSet{}
	}
	if err := r.List(ctx, list, client.InNamespace(v.Namespace), client.MatchingFields{templateindex.VersionOwner: string(v.UID)}); err != nil {
		return false, err
	}
	objects, err := meta.ExtractList(list)
	if err != nil {
		return false, err
	}
	seenExpected := false
	for _, object := range objects {
		candidate, ok := object.(client.Object)
		if !ok {
			return false, fmt.Errorf("unexpected companion object type")
		}
		if candidate.GetName() == v.Name {
			seenExpected = true
		}
		done, err := r.deleteKnownCompanion(ctx, v, candidate, !autoscalers)
		if err != nil || !done {
			return done, err
		}
	}
	if !seenExpected {
		return r.deleteKnownCompanion(ctx, v, expected, !autoscalers)
	}
	return true, nil
}
func (r *Reconciler) cleanupVersion(ctx context.Context, v *api.SandboxTemplate) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(v, api.SandboxTemplateCleanupFinalizer) {
		return ctrl.Result{}, nil
	}
	done, err := r.deleteOwnedCompanions(ctx, v, true)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !done {
		return ctrl.Result{RequeueAfter: retryInterval}, nil
	}
	done, err = r.deleteOwnedCompanions(ctx, v, false)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !done {
		return ctrl.Result{RequeueAfter: retryInterval}, nil
	}
	// Inventory has a non-controller blocking version owner in addition to the
	// pool owner. Delivery removes ALL owners through the existing claim path.
	// Now that autoscaler/pool are gone, foreground GC of the version is the
	// persistent completion barrier for inventory -> Pods/PVCs, including items
	// not yet observed in any one replica cache. No real List is needed.
	if v.Annotations[templatecreation.CleanupForegroundAnnotation] != "true" {
		if err := r.Delete(ctx, v, client.Preconditions{UID: &v.UID, ResourceVersion: &v.ResourceVersion}, client.PropagationPolicy(metav1.DeletePropagationForeground)); err != nil && !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
		// GC may remove foregroundDeletion immediately when there is no stock.
		// Persist an acknowledgement after the accepted foreground Delete so a
		// restart can distinguish completed GC from an unrequested GC phase.
		// A UID-bound partial merge avoids overwriting annotations or racing GC's
		// resource-version update; UID itself is immutable in Kubernetes updates.
		payload, err := json.Marshal(map[string]any{"metadata": map[string]any{"uid": v.UID, "annotations": map[string]string{templatecreation.CleanupForegroundAnnotation: "true"}}})
		if err != nil {
			return ctrl.Result{}, err
		}
		if err := r.Patch(ctx, v, client.RawPatch(types.MergePatchType, payload)); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: retryInterval}, nil
	}
	before := v.DeepCopy()
	controllerutil.RemoveFinalizer(v, api.SandboxTemplateCleanupFinalizer)
	if err := r.Patch(ctx, v, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: retryInterval}, nil
}

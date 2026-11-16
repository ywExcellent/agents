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
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/validation/field"
	sigsjson "sigs.k8s.io/json"

	api "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/templateconfig"
)

type workloadInput struct {
	corev1.PodSpec
	VolumeClaimTemplates []corev1.PersistentVolumeClaim `json:"volumeClaimTemplates,omitempty"`
	PersistentContents   []string                       `json:"persistentContents,omitempty"`
	Runtimes             []api.RuntimeConfig            `json:"runtimes,omitempty"`
	PauseStrategy        *api.PauseStrategy             `json:"pauseStrategy,omitempty"`
	Probes               []api.Probe                    `json:"probes,omitempty"`
	AutoPausePolicy      *api.AutoPausePolicy           `json:"autoPausePolicy,omitempty"`
	Lifecycle            *api.SandboxLifecycle          `json:"lifecycle,omitempty"`
}
type optionsInput struct {
	WarmPool *api.SandboxTemplateWarmPoolOptions `json:"warmPool,omitempty"`
}

// Only business maps are transport inputs; native ObjectMeta is not exposed.
type metadataInput struct {
	Labels      map[string]string `json:"labels,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`
}
type createInput struct {
	Metadata *metadataInput `json:"metadata,omitempty"`
	Name     string         `json:"name"`
	Spec     *workloadInput `json:"spec"`
	Options  *optionsInput  `json:"options,omitempty"`
}

func decodeCreate(body io.Reader) (createInput, error) {
	var in createInput
	raw, err := io.ReadAll(io.LimitReader(body, 2*1024*1024+1))
	if err != nil {
		return in, err
	}
	if len(raw) > 2*1024*1024 {
		return in, fmt.Errorf("request body too large")
	}
	// Kubernetes strict decoding: case-sensitive names, no unknown or duplicate fields.
	strict, err := sigsjson.UnmarshalStrict(raw, &in)
	if err != nil {
		return in, err
	}
	if len(strict) > 0 {
		return in, errors.Join(strict...)
	}
	if in.Name == "" || in.Spec == nil {
		return in, fmt.Errorf("name and spec are required")
	}
	var root map[string]json.RawMessage
	if err := json.Unmarshal(raw, &root); err != nil {
		return in, err
	}
	// A null string is decoded as an empty Go string. Reject it explicitly,
	// and reject null maps rather than silently losing declared metadata.
	if value := root["metadata"]; len(value) > 0 && !bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
		var metadata map[string]json.RawMessage
		if err := json.Unmarshal(value, &metadata); err != nil {
			return in, err
		}
		for key, value := range metadata {
			if bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
				return in, fmt.Errorf("metadata.%s must be a string map", key)
			}
			var entries map[string]json.RawMessage
			if err := json.Unmarshal(value, &entries); err != nil {
				return in, err
			}
			for _, entry := range entries {
				if bytes.Equal(bytes.TrimSpace(entry), []byte("null")) {
					return in, fmt.Errorf("metadata values must be strings")
				}
			}
		}
		if errs := templateconfig.ValidateMetadata(templateconfig.Metadata{Labels: in.Metadata.Labels, Annotations: in.Metadata.Annotations}, field.NewPath("metadata")); len(errs) > 0 {
			return in, errs.ToAggregate()
		}
	}
	// Go decoding erases explicit null/zero presence. Inspect the wire declaration
	// before defaults, especially disabled pools and integer scalar options.
	var opts, warm map[string]json.RawMessage
	if value := root["options"]; len(value) > 0 && !bytes.Equal(value, []byte("null")) {
		if err := json.Unmarshal(value, &opts); err != nil {
			return in, err
		}
	}
	if value := opts["warmPool"]; len(value) > 0 && !bytes.Equal(value, []byte("null")) {
		if err := json.Unmarshal(value, &warm); err != nil {
			return in, err
		}
	}
	if in.Options != nil && in.Options.WarmPool != nil {
		w := in.Options.WarmPool
		for _, key := range []string{"enabled", "size"} {
			if value, ok := warm[key]; ok && bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
				return in, fmt.Errorf("%s must not be null", key)
			}
		}
		if w.Enabled != nil && !*w.Enabled {
			if _, ok := warm["size"]; ok {
				return in, fmt.Errorf("disabled warmPool must omit size")
			}
			if _, ok := warm["autoscaler"]; ok {
				return in, fmt.Errorf("disabled warmPool must omit autoscaler")
			}
		}
		if w.Autoscaler != nil {
			var auto map[string]json.RawMessage
			if err := json.Unmarshal(warm["autoscaler"], &auto); err != nil {
				return in, err
			}
			for _, key := range []string{"minReplicas", "maxReplicas"} {
				if value, ok := auto[key]; ok && bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
					return in, fmt.Errorf("%s must not be null", key)
				}
			}
		}
	}
	// Native ObjectMeta is a persistence type, not permission to configure PVC
	// labels, annotations, namespace, UID, ownerReferences or system fields.
	var spec map[string]json.RawMessage
	if err := json.Unmarshal(root["spec"], &spec); err != nil {
		return in, err
	}
	if value, ok := spec["lifecycle"]; ok && !bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
		var actions map[string]json.RawMessage
		if err := json.Unmarshal(value, &actions); err != nil {
			return in, err
		}
		for key := range actions {
			if key != "prePause" && key != "postResume" {
				return in, fmt.Errorf("unsupported template lifecycle hook")
			}
		}
	}
	if value, ok := spec["volumeClaimTemplates"]; ok {
		var pvcs []map[string]json.RawMessage
		if err := json.Unmarshal(value, &pvcs); err != nil {
			return in, err
		}
		for _, pvc := range pvcs {
			for key := range pvc {
				if key != "metadata" && key != "spec" {
					return in, fmt.Errorf("PVC only accepts metadata and spec")
				}
			}
			var metadata map[string]json.RawMessage
			if err := json.Unmarshal(pvc["metadata"], &metadata); err != nil {
				return in, err
			}
			for key := range metadata {
				if key != "name" {
					return in, fmt.Errorf("PVC metadata only accepts name")
				}
			}
		}
	}
	return in, nil
}
func (in createInput) definition() templateconfig.Definition {
	s := in.Spec
	d := templateconfig.Definition{Pod: s.PodSpec, VolumeClaimTemplates: s.VolumeClaimTemplates, PersistentContents: s.PersistentContents, Runtimes: s.Runtimes, PauseStrategy: s.PauseStrategy, Probes: s.Probes, AutoPausePolicy: s.AutoPausePolicy, Lifecycle: s.Lifecycle}
	if in.Metadata != nil {
		d.Metadata = templateconfig.Metadata{Labels: in.Metadata.Labels, Annotations: in.Metadata.Annotations}.Clone()
	}
	if in.Options != nil {
		d.WarmPool = in.Options.WarmPool
	}
	return d
}

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
	"fmt"
	"io"
	"reflect"

	corev1 "k8s.io/api/core/v1"

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
type createInput struct {
	Name    string         `json:"name"`
	Spec    *workloadInput `json:"spec"`
	Options *optionsInput  `json:"options,omitempty"`
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
	if err := rejectDuplicateKeys(json.NewDecoder(bytes.NewReader(raw))); err != nil {
		return in, err
	}
	if err := strictShape(raw, reflect.TypeFor[createInput](), ""); err != nil {
		return in, err
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&in); err != nil {
		return in, err
	}
	if in.Name == "" || in.Spec == nil {
		return in, fmt.Errorf("name and spec are required")
	}
	var root map[string]json.RawMessage
	if err := json.Unmarshal(raw, &root); err != nil {
		return in, err
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
	if in.Options != nil {
		d.WarmPool = in.Options.WarmPool
	}
	return d
}
func rejectDuplicateKeys(d *json.Decoder) error {
	d.UseNumber()
	var value func() error
	value = func() error {
		token, err := d.Token()
		if err != nil {
			return err
		}
		delim, ok := token.(json.Delim)
		if !ok {
			return nil
		}
		if delim == '{' {
			keys := map[string]bool{}
			for d.More() {
				token, err := d.Token()
				if err != nil {
					return err
				}
				key, ok := token.(string)
				if !ok {
					return fmt.Errorf("invalid JSON object key")
				}
				if keys[key] {
					return fmt.Errorf("duplicate JSON field")
				}
				keys[key] = true
				if err := value(); err != nil {
					return err
				}
			}
		} else if delim == '[' {
			for d.More() {
				if err := value(); err != nil {
					return err
				}
			}
		} else {
			return fmt.Errorf("unexpected JSON delimiter")
		}
		_, err = d.Token()
		return err
	}
	if err := value(); err != nil {
		return err
	}
	_, err := d.Token()
	if err != io.EOF {
		return fmt.Errorf("request must contain one JSON value")
	}
	return nil
}

// strictShape preserves exact field spelling (encoding/json otherwise accepts
// case-insensitive names). It recursively checks native structs, but respects
// custom scalar unmarshalers such as IntOrString and Kubernetes quantities.
func strictShape(raw json.RawMessage, t reflect.Type, path string) error {
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil
	}
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if reflect.PointerTo(t).Implements(reflect.TypeFor[json.Unmarshaler]()) {
		return nil
	}
	switch t.Kind() {
	case reflect.Struct:
		var object map[string]json.RawMessage
		if err := json.Unmarshal(raw, &object); err != nil {
			return err
		}
		fields := map[string]reflect.Type{}
		var collect func(reflect.Type)
		collect = func(st reflect.Type) {
			for i := 0; i < st.NumField(); i++ {
				f := st.Field(i)
				if !f.IsExported() {
					continue
				}
				tag := f.Tag.Get("json")
				name := tag
				for j, c := range tag {
					if c == ',' {
						name = tag[:j]
						break
					}
				}
				if name == "-" {
					continue
				}
				if f.Anonymous && name == "" {
					ft := f.Type
					for ft.Kind() == reflect.Pointer {
						ft = ft.Elem()
					}
					if ft.Kind() == reflect.Struct {
						collect(ft)
						continue
					}
				}
				if name == "" {
					name = f.Name
				}
				fields[name] = f.Type
			}
		}
		collect(t)
		for name, value := range object {
			ft, ok := fields[name]
			if !ok {
				return fmt.Errorf("unsupported field %s%s", path, name)
			}
			if err := strictShape(value, ft, path+name+"."); err != nil {
				return err
			}
		}
	case reflect.Slice, reflect.Array:
		if t.Elem().Kind() == reflect.Uint8 {
			return nil
		}
		var array []json.RawMessage
		if err := json.Unmarshal(raw, &array); err != nil {
			return err
		}
		for _, value := range array {
			if err := strictShape(value, t.Elem(), path); err != nil {
				return err
			}
		}
	case reflect.Map:
		var object map[string]json.RawMessage
		if err := json.Unmarshal(raw, &object); err != nil {
			return err
		}
		for _, value := range object {
			if err := strictShape(value, t.Elem(), path); err != nil {
				return err
			}
		}
	}
	return nil
}

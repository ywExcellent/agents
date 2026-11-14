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
package main

import (
	"testing"

	"github.com/openkruise/agents/pkg/sandboxroute/refresh"
)

func TestValidateCustomTemplatePort(t *testing.T) {
	for _, test := range []struct {
		port, metrics, memberlist int
		valid                     bool
	}{
		{port: 0, valid: true}, {port: 8081, valid: true}, {port: -1}, {port: 65536}, {port: 8080}, {port: 8081, metrics: 8081}, {port: 7946}, {port: refresh.DefaultPort}, {port: 9002},
	} {
		err := validateCustomTemplatePort(test.port, 8080, test.metrics, test.memberlist)
		if (err == nil) != test.valid {
			t.Fatalf("port=%d error=%v", test.port, err)
		}
	}
}

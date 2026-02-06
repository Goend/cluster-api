/*
Copyright 2026 The Kubernetes Authors.

Licensed under the the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package v1alpha1 contains API Schema definitions for the ansible control plane v1alpha1 API group.
// +kubebuilder:object:generate=true
// +groupName=controlplane.cluster.x-k8s.io
package v1alpha1

import (
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/scheme"
)

// GroupVersion is group version used to register these objects.
var GroupVersion = schema.GroupVersion{Group: "controlplane.cluster.x-k8s.io", Version: "v1alpha1"}

var (
	// SchemeBuilder collects the Go types that should be added to the runtime.Scheme.
	SchemeBuilder = &scheme.Builder{GroupVersion: GroupVersion}

	// AddToScheme adds the registered types to the supplied scheme.
	AddToScheme = SchemeBuilder.AddToScheme
)

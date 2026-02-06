package config

import (
	"context"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic/fake"
)

func TestRendererResourceFunction(t *testing.T) {
	scheme := runtime.NewScheme()
	configsGVR := schema.GroupVersionResource{
		Group:    "controlplane.cluster.x-k8s.io",
		Version:  "v1alpha1",
		Resource: "configs",
	}
	infraGVR := schema.GroupVersionResource{
		Group:    "infra.cluster.x-k8s.io",
		Version:  "v1alpha1",
		Resource: "infratemplates",
	}
	infra := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "infra.cluster.x-k8s.io/v1alpha1",
			"kind":       "InfraTemplate",
			"metadata": map[string]interface{}{
				"name":      "net-a",
				"namespace": "ems",
			},
			"spec": map[string]interface{}{
				"network": map[string]interface{}{
					"cidr": "192.168.0.0/24",
				},
			},
		},
	}
	dyn := fake.NewSimpleDynamicClientWithCustomListKinds(
		scheme,
		map[schema.GroupVersionResource]string{
			configsGVR: "ConfigList",
			infraGVR:   "InfraTemplateList",
		},
		infra,
	)
	renderer := NewRenderer(dyn, fakeRESTMapper(), "ems", nil)
	if err := renderer.Load(context.Background()); err != nil {
		t.Fatalf("load failed: %v", err)
	}
	template := `cidr={{ eval "resource('infra.cluster.x-k8s.io','v1alpha1','infratemplates','net-a')['spec']['network']['cidr']" }}`
	out, err := renderer.Render(context.Background(), template)
	if err != nil {
		t.Fatalf("render failed: %v", err)
	}
	if out != "cidr=192.168.0.0/24" {
		t.Fatalf("unexpected resource fetch result: %s", out)
	}
}

func TestRendererGoTemplateFeatures(t *testing.T) {
	scheme := runtime.NewScheme()
	configsGVR := schema.GroupVersionResource{
		Group:    "controlplane.cluster.x-k8s.io",
		Version:  "v1alpha1",
		Resource: "configs",
	}
	infraGVR := schema.GroupVersionResource{
		Group:    "infra.cluster.x-k8s.io",
		Version:  "v1alpha1",
		Resource: "infratemplates",
	}
	obj := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "infra.cluster.x-k8s.io/v1alpha1",
			"kind":       "InfraTemplate",
			"metadata": map[string]interface{}{
				"name":      "net-b",
				"namespace": "ems",
			},
			"spec": map[string]interface{}{
				"items": []interface{}{"alpha", "beta", "gamma"},
			},
		},
	}
	dyn := fake.NewSimpleDynamicClientWithCustomListKinds(scheme, map[schema.GroupVersionResource]string{
		configsGVR: "ConfigList",
		infraGVR:   "InfraTemplateList",
	}, obj)
	renderer := NewRenderer(dyn, fakeRESTMapper(), "ems", nil)
	if err := renderer.Load(context.Background()); err != nil {
		t.Fatalf("load failed: %v", err)
	}
	template := `{{ $res := eval "resource('infra.cluster.x-k8s.io','v1alpha1','infratemplates','net-b')" }}{{ range $idx, $item := index $res "spec" "items" }}{{$idx}}={{$item}};{{ end }}count={{ len (index $res "spec" "items") }}`
	out, err := renderer.Render(context.Background(), template)
	if err != nil {
		t.Fatalf("render failed: %v", err)
	}
	if out != "0=alpha;1=beta;2=gamma;count=3" {
		t.Fatalf("unexpected range output: %s", out)
	}
	templateList := `{{ $list := eval "resource('infra.cluster.x-k8s.io','v1alpha1','infratemplates')" }}{{ len (index $list "items") }}`
	out, err = renderer.Render(context.Background(), templateList)
	if err != nil {
		t.Fatalf("render list template failed: %v", err)
	}
	if out != "1" {
		t.Fatalf("unexpected list size: %s", out)
	}
}

func TestRendererResourceNamespaceOverride(t *testing.T) {
	scheme := runtime.NewScheme()
	configsGVR := schema.GroupVersionResource{
		Group:    "controlplane.cluster.x-k8s.io",
		Version:  "v1alpha1",
		Resource: "configs",
	}
	infraGVR := schema.GroupVersionResource{
		Group:    "infra.cluster.x-k8s.io",
		Version:  "v1alpha1",
		Resource: "infratemplates",
	}
	obj := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "infra.cluster.x-k8s.io/v1alpha1",
			"kind":       "InfraTemplate",
			"metadata": map[string]interface{}{
				"name":      "net-ns",
				"namespace": "prod",
			},
			"spec": map[string]interface{}{
				"network": map[string]interface{}{
					"cidr": "172.16.0.0/16",
				},
			},
		},
	}
	dyn := fake.NewSimpleDynamicClientWithCustomListKinds(scheme, map[schema.GroupVersionResource]string{
		configsGVR: "ConfigList",
		infraGVR:   "InfraTemplateList",
	}, obj)
	renderer := NewRenderer(dyn, fakeRESTMapper(), "ems", nil)
	if err := renderer.Load(context.Background()); err != nil {
		t.Fatalf("load failed: %v", err)
	}
	template := `cidr={{ eval "resource('infra.cluster.x-k8s.io','v1alpha1','infratemplates','net-ns','prod')['spec']['network']['cidr']" }}`
	out, err := renderer.Render(context.Background(), template)
	if err != nil {
		t.Fatalf("render failed: %v", err)
	}
	if out != "cidr=172.16.0.0/16" {
		t.Fatalf("unexpected value: %s", out)
	}
	templateList := `{{ $list := eval "resource('infra.cluster.x-k8s.io','v1alpha1','infratemplates','','prod')" }}{{ len (index $list "items") }}`
	out, err = renderer.Render(context.Background(), templateList)
	if err != nil {
		t.Fatalf("list render failed: %v", err)
	}
	if out != "1" {
		t.Fatalf("unexpected list length: %s", out)
	}
}

func TestRendererClusterScopedResource(t *testing.T) {
	scheme := runtime.NewScheme()
	configsGVR := schema.GroupVersionResource{
		Group:    "controlplane.cluster.x-k8s.io",
		Version:  "v1alpha1",
		Resource: "configs",
	}
	globalGVR := schema.GroupVersionResource{
		Group:    "core.cluster.x-k8s.io",
		Version:  "v1alpha1",
		Resource: "globalconfigs",
	}
	global := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "core.cluster.x-k8s.io/v1alpha1",
			"kind":       "GlobalConfig",
			"metadata": map[string]interface{}{
				"name": "default",
			},
			"data": map[string]interface{}{
				"foo": "bar",
			},
		},
	}
	dyn := fake.NewSimpleDynamicClientWithCustomListKinds(scheme, map[schema.GroupVersionResource]string{
		configsGVR: "ConfigList",
		globalGVR:  "GlobalConfigList",
	}, global)
	renderer := NewRenderer(dyn, fakeRESTMapper(), "ems", nil)
	if err := renderer.Load(context.Background()); err != nil {
		t.Fatalf("load failed: %v", err)
	}
	template := `value={{ eval "resource('core.cluster.x-k8s.io','v1alpha1','globalconfigs','default')['data']['foo']" }}`
	out, err := renderer.Render(context.Background(), template)
	if err != nil {
		t.Fatalf("render failed: %v", err)
	}
	if out != "value=bar" {
		t.Fatalf("unexpected value: %s", out)
	}
}

func TestRendererSetValueAndTemplateFuncs(t *testing.T) {
	scheme := runtime.NewScheme()
	configsGVR := schema.GroupVersionResource{
		Group:    "controlplane.cluster.x-k8s.io",
		Version:  "v1alpha1",
		Resource: "configs",
	}
	dyn := fake.NewSimpleDynamicClientWithCustomListKinds(scheme, map[schema.GroupVersionResource]string{
		configsGVR: "ConfigList",
	})
	renderer := NewRenderer(dyn, fakeRESTMapper(), "ems", nil)
	if err := renderer.Load(context.Background()); err != nil {
		t.Fatalf("load failed: %v", err)
	}
	renderer.SetValue("__machine", map[string]interface{}{
		"metadata": map[string]interface{}{
			"name": "machine-1",
		},
		"spec": map[string]interface{}{
			"version": "v1.29.0",
		},
	})
	template := `machine:
{{ indent 2 (toYAML (eval "__machine")) }}`
	out, err := renderer.Render(context.Background(), template)
	if err != nil {
		t.Fatalf("render failed: %v", err)
	}
	expected := "machine:\n  metadata:\n    name: machine-1\n  spec:\n    version: v1.29.0"
	if out != expected {
		t.Fatalf("unexpected output: %s", out)
	}
}

func newConfigObject(name string, data map[string]interface{}) runtime.Object {
	return &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "controlplane.cluster.x-k8s.io/v1alpha1",
			"kind":       "Config",
			"metadata": map[string]interface{}{
				"name":      name,
				"namespace": "ems",
			},
			"data": data,
		},
	}
}

func fakeRESTMapper() meta.RESTMapper {
	mapper := meta.NewDefaultRESTMapper([]schema.GroupVersion{
		{Group: "controlplane.cluster.x-k8s.io", Version: "v1alpha1"},
		{Group: "infra.cluster.x-k8s.io", Version: "v1alpha1"},
		{Group: "core.cluster.x-k8s.io", Version: "v1alpha1"},
	})
	mapper.Add(schema.GroupVersionKind{
		Group:   "controlplane.cluster.x-k8s.io",
		Version: "v1alpha1",
		Kind:    "Config",
	}, meta.RESTScopeNamespace)
	mapper.Add(schema.GroupVersionKind{
		Group:   "infra.cluster.x-k8s.io",
		Version: "v1alpha1",
		Kind:    "InfraTemplate",
	}, meta.RESTScopeNamespace)
	mapper.Add(schema.GroupVersionKind{
		Group:   "core.cluster.x-k8s.io",
		Version: "v1alpha1",
		Kind:    "GlobalConfig",
	}, meta.RESTScopeRoot)
	return mapper
}

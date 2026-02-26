package controllers

import (
	"encoding/json"
	"fmt"

	"github.com/pkg/errors"
	"k8s.io/apimachinery/pkg/runtime"

	bootstrapv1 "sigs.k8s.io/cluster-api/bootstrap/ansible/api/v1alpha1"
)

func buildClusterResourceTemplate(scope *Scope) (bootstrapv1.ResourceTemplate, error) {
	if scope.Config.Spec.ClusterTemplate != nil {
		return clusterResourceFromTemplate(scope, scope.Config.Spec.ClusterTemplate)
	}
	ref := scope.Config.Spec.ClusterRef
	if ref.APIVersion == "" || ref.Kind == "" || ref.Name == "" {
		return bootstrapv1.ResourceTemplate{}, errors.New("cluster template or clusterRef must define apiVersion, kind, and name")
	}
	if ref.Namespace == "" {
		ref.Namespace = scope.Config.Namespace
	}
	return ref, nil
}

func buildClusterOperationResourceTemplate(scope *Scope, clusterTemplate bootstrapv1.ResourceTemplate) (bootstrapv1.ResourceTemplate, error) {
	if scope.Config.Spec.ClusterOperationTemplate != nil {
		return clusterOperationResourceFromTemplate(scope, scope.Config.Spec.ClusterOperationTemplate, clusterTemplate)
	}
	ref := scope.Config.Spec.ClusterOpsRef
	if ref.APIVersion == "" || ref.Kind == "" || ref.Name == "" {
		return bootstrapv1.ResourceTemplate{}, errors.New("cluster operation template or clusterOpsRef must define apiVersion, kind, and name")
	}
	if ref.Namespace == "" {
		ref.Namespace = scope.Config.Namespace
	}
	return ref, nil
}

func clusterResourceFromTemplate(scope *Scope, template *bootstrapv1.KubeanClusterTemplate) (bootstrapv1.ResourceTemplate, error) {
	if template.APIVersion == "" || template.Kind == "" {
		return bootstrapv1.ResourceTemplate{}, errors.New("clusterTemplate requires apiVersion and kind")
	}
	rendered := bootstrapv1.ResourceTemplate{
		APIVersion:  template.APIVersion,
		Kind:        template.Kind,
		Namespace:   template.Namespace,
		Labels:      copyStringMap(template.Labels),
		Annotations: copyStringMap(template.Annotations),
		Name:        kubeanClusterObjectName(scope),
	}
	if rendered.Namespace == "" {
		rendered.Namespace = scope.Config.Namespace
	}
	spec := defaultKubeanClusterSpec(scope, rendered.Namespace)
	if len(template.Spec.Raw) > 0 {
		userSpec, err := decodeTemplateSpec(template.Spec)
		if err != nil {
			return bootstrapv1.ResourceTemplate{}, errors.Wrap(err, "failed to decode cluster template spec")
		}
		spec = mergeSpecMaps(spec, userSpec)
	}
	raw, err := json.Marshal(spec)
	if err != nil {
		return bootstrapv1.ResourceTemplate{}, errors.Wrap(err, "failed to encode cluster template spec")
	}
	rendered.Spec = runtime.RawExtension{Raw: raw}
	return rendered, nil
}

func clusterOperationResourceFromTemplate(scope *Scope, template *bootstrapv1.KubeanClusterOperationTemplate, clusterTemplate bootstrapv1.ResourceTemplate) (bootstrapv1.ResourceTemplate, error) {
	if template.APIVersion == "" || template.Kind == "" {
		return bootstrapv1.ResourceTemplate{}, errors.New("clusterOperationTemplate requires apiVersion and kind")
	}
	rendered := bootstrapv1.ResourceTemplate{
		APIVersion:  template.APIVersion,
		Kind:        template.Kind,
		Namespace:   template.Namespace,
		Labels:      copyStringMap(template.Labels),
		Annotations: copyStringMap(template.Annotations),
		Name:        defaultClusterOperationName(scope),
	}
	if rendered.Namespace == "" {
		rendered.Namespace = scope.Config.Namespace
	}
	spec := defaultKubeanClusterOperationSpec(scope, rendered.Namespace, clusterTemplate)
	if len(template.Spec.Raw) > 0 {
		userSpec, err := decodeTemplateSpec(template.Spec)
		if err != nil {
			return bootstrapv1.ResourceTemplate{}, errors.Wrap(err, "failed to decode cluster operation template spec")
		}
		spec = mergeSpecMaps(spec, userSpec)
	}
	raw, err := json.Marshal(spec)
	if err != nil {
		return bootstrapv1.ResourceTemplate{}, errors.Wrap(err, "failed to encode cluster operation template spec")
	}
	rendered.Spec = runtime.RawExtension{Raw: raw}
	return rendered, nil
}

func defaultKubeanClusterSpec(scope *Scope, namespace string) map[string]interface{} {
	// kubean.io/v1alpha1 Cluster 仅接受 {name, namespace}
	return map[string]interface{}{
		"hostsConfRef": map[string]interface{}{
			"name":      kubeanHostsConfigMapName(scope),
			"namespace": namespace,
		},
		"varsConfRef": map[string]interface{}{
			"name":      kubeanVarsConfigMapName(scope),
			"namespace": namespace,
		},
		"sshAuthRef": map[string]interface{}{
			"name":      kubeanSSHAuthSecretName(scope),
			"namespace": namespace,
		},
	}
}

func defaultKubeanClusterOperationSpec(scope *Scope, namespace string, clusterTemplate bootstrapv1.ResourceTemplate) map[string]interface{} {
	// kubean.io/v1alpha1 expects string fields:
	// - spec.cluster: Cluster name (string)
	// - spec.actionType: Operation type (string)
	clusterName := clusterTemplate.Name
	if clusterName == "" {
		clusterName = kubeanClusterObjectName(scope)
	}

	action := scope.ClusterOperationPlan.ActionType
	if action == "" {
		action = bootstrapv1.ClusterOperationActionCluster
	}

	return map[string]interface{}{
		"cluster": clusterName,
		"action":  action,
	}
}

func kubeanClusterObjectName(scope *Scope) string {
	if scope.Cluster != nil && scope.Cluster.Name != "" {
		return scope.Cluster.Name
	}
	return scope.Config.Name
}

func kubeanHostsConfigMapName(scope *Scope) string {
	return fmt.Sprintf("%s-hosts-conf", kubeanClusterObjectName(scope))
}

func kubeanVarsConfigMapName(scope *Scope) string {
	return fmt.Sprintf("%s-vars-conf", kubeanClusterObjectName(scope))
}

func kubeanSSHAuthSecretName(scope *Scope) string {
	return fmt.Sprintf("%s-ssh-auth", kubeanClusterObjectName(scope))
}

func defaultClusterOperationName(scope *Scope) string {
	return fmt.Sprintf("%s-%s", kubeanClusterObjectName(scope), scope.ConfigOwner.GetName())
}

func copyStringMap(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func copyInterfaceMap(in map[string]interface{}) map[string]interface{} {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]interface{}, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func mergeSpecMaps(base, overrides map[string]interface{}) map[string]interface{} {
	if base == nil {
		base = map[string]interface{}{}
	}
	for k, v := range overrides {
		srcMap, srcIsMap := v.(map[string]interface{})
		dstMap, dstIsMap := base[k].(map[string]interface{})
		if srcIsMap {
			if !dstIsMap {
				dstMap = map[string]interface{}{}
			}
			base[k] = mergeSpecMaps(dstMap, srcMap)
			continue
		}
		base[k] = v
	}
	return base
}

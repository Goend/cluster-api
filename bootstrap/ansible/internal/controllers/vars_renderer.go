package controllers

import (
	"context"
	"fmt"

	"github.com/pkg/errors"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/yaml"

	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	configrenderer "sigs.k8s.io/cluster-api/controlplane/ansible/upstream/config"
)

const (
	clusterNameValueKey     = "__cluster_name"
	ansibleConfigValueKey   = "__ansible_config"
	machineNameValueKey     = "__machine_name"
	machineSectionValueKey  = "__machine_section"
	infraSectionValueKey    = "__infra_section"
	businessSectionValueKey = "__business_section"
	fixedSectionValueKey    = "__fixed_section"

	defaultVarsTemplate = `
cluster_name: {{ eval "__cluster_name" }}
ansible_config: {{ eval "__ansible_config" }}
machine_name: {{ eval "__machine_name" }}
machine_control_plane:
{{ indent 2 (toYAML (eval "__machine_section")) }}
infrastructure_provider:
{{ indent 2 (toYAML (eval "__infra_section")) }}
business_config:
{{ indent 2 (toYAML (eval "__business_section")) }}
fixed_config:
{{ indent 2 (toYAML (eval "__fixed_section")) }}
`
)

type fieldMapping struct {
	key  string
	path []string
}

var (
	clusterInfraStringFields = []fieldMapping{
		{"kube_network_plugin", []string{"status", "network", "plugin"}},
		{"cilium_openstack_project_id", []string{"status", "network", "openStack", "projectID"}},
		{"cilium_openstack_default_subnet_id", []string{"status", "network", "openStack", "defaultSubnetID"}},
		{"master_virtual_vip", []string{"status", "loadBalancer", "controlPlane", "vip"}},
		{"ingress_virtual_vip", []string{"status", "loadBalancer", "ingress", "vip"}},
		{"keepalived_interface", []string{"status", "network", "interface", "default"}},
		{"harbor_addr", []string{"status", "loadBalancer", "ingress", "vip"}},
		{"cloud_master_vip", []string{"status", "platform", "controlPlaneVIP"}},
		{"openstack_auth_domain", []string{"status", "platform", "openStack", "keystoneEndpoint"}},
		{"openstack_cinder_domain", []string{"status", "platform", "openStack", "cinderEndpoint"}},
		{"openstack_nova_domain", []string{"status", "platform", "openStack", "novaEndpoint"}},
		{"openstack_neutron_domain", []string{"status", "platform", "openStack", "neutronEndpoint"}},
		{"openstack_user_name", []string{"spec", "credentials", "openStack", "username"}},
		{"openstack_project_name", []string{"status", "platform", "openStack", "project"}},
		{"openstack_project_domain_name", []string{"status", "platform", "openStack", "projectDomain"}},
		{"openstack_user_app_cred_name", []string{"status", "platform", "openStack", "applicationCredentialName"}},
		{"openstack_region_name", []string{"status", "platform", "openStack", "region"}},
		{"ntp_server", []string{"status", "network", "timeServer"}},
		{"vip_mgmt", []string{"status", "loadBalancer", "management", "vip"}},
		{"flannel_interface", []string{"status", "network", "interface", "flannel"}},
	}
	clusterInfraBoolFields = []fieldMapping{
		{"vpc_cni_webhook_enable", []string{"spec", "network", "features", "vpcCniWebhook"}},
	}
	clusterInfraSliceFields = []fieldMapping{
		{"cilium_openstack_security_group_ids", []string{"status", "network", "openStack", "securityGroupIDs"}},
	}
	machineInfraNodeResourcesPath = []string{"spec", "resources", "reserved"}
	configGVR                     = schema.GroupVersionResource{
		Group:    "controlplane.cluster.x-k8s.io",
		Version:  "v1alpha1",
		Resource: "configs",
	}
)

func (r *AnsibleConfigReconciler) renderVarsConfig(ctx context.Context, scope *Scope) (string, error) {
	if r.DynamicClient == nil || r.RESTMapper == nil {
		return "", errors.New("vars renderer is not configured")
	}

	machine, err := machineFromScope(scope)
	if err != nil {
		return "", err
	}
	if machine == nil {
		return "", errors.New("machine owner is required to render vars")
	}

	machineSection := buildMachineVars(machine)
	infraSection, err := r.buildInfrastructureVars(ctx, scope, machine)
	if err != nil {
		return "", err
	}

	infraOverrides, err := r.loadConfigSection(ctx, scope, "infra")
	if err != nil {
		return "", err
	}
	infraSection = mergeSectionMaps(infraSection, infraOverrides)

	businessSection := defaultBusinessConfig(scope)
	businessOverrides, err := r.loadConfigSection(ctx, scope, "business")
	if err != nil {
		return "", err
	}
	businessSection = mergeSectionMaps(businessSection, businessOverrides)

	fixedSection := defaultFixedConfig()
	fixedOverrides, err := r.loadConfigSection(ctx, scope, "fixed")
	if err != nil {
		return "", err
	}
	fixedSection = mergeSectionMaps(fixedSection, fixedOverrides)

	renderer := configrenderer.NewRenderer(r.DynamicClient, r.RESTMapper, scope.Config.Namespace, nil)
	renderer.SetValue(clusterNameValueKey, scope.Cluster.Name)
	renderer.SetValue(ansibleConfigValueKey, scope.Config.Name)
	renderer.SetValue(machineNameValueKey, scope.ConfigOwner.GetName())
	renderer.SetValue(machineSectionValueKey, machineSection)
	renderer.SetValue(infraSectionValueKey, infraSection)
	renderer.SetValue(businessSectionValueKey, businessSection)
	renderer.SetValue(fixedSectionValueKey, fixedSection)

	if err := renderer.Load(ctx); err != nil {
		return "", err
	}
	return renderer.Render(ctx, defaultVarsTemplate)
}

func (r *AnsibleConfigReconciler) buildInfrastructureVars(ctx context.Context, scope *Scope, machine *clusterv1.Machine) (map[string]interface{}, error) {
	result := map[string]interface{}{}
	if scope.Cluster == nil {
		return result, nil
	}

	if scope.Cluster.Spec.InfrastructureRef != nil {
		infraObj, err := r.fetchObjectForRef(ctx, scope.Cluster.Spec.InfrastructureRef, scope.Cluster.Namespace)
		if err != nil {
			return nil, err
		}
		if infraObj != nil {
			applyStringMappings(result, infraObj.Object, clusterInfraStringFields)
			applyBoolMappings(result, infraObj.Object, clusterInfraBoolFields)
			applySliceMappings(result, infraObj.Object, clusterInfraSliceFields)
			if err := r.assignOpenStackSecret(ctx, scope, infraObj, result); err != nil {
				return nil, err
			}
		}
	}

	nodeResources, err := r.aggregateNodeResources(ctx, scope, machine)
	if err != nil {
		return nil, err
	}
	if len(nodeResources) > 0 {
		result["node_resources"] = nodeResources
	}

	return result, nil
}

func (r *AnsibleConfigReconciler) loadConfigSection(ctx context.Context, scope *Scope, category string) (map[string]interface{}, error) {
	result := map[string]interface{}{}
	if r.DynamicClient == nil {
		return result, nil
	}
	name := fmt.Sprintf("%s-vars-%s", kubeanClusterObjectName(scope), category)
	obj, err := r.DynamicClient.Resource(configGVR).Namespace(scope.Config.Namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return result, nil
		}
		return nil, err
	}
	raw, _, _ := unstructured.NestedFieldNoCopy(obj.Object, "data")
	return normalizeConfigData(raw), nil
}

func buildMachineVars(machine *clusterv1.Machine) map[string]interface{} {
	result := map[string]interface{}{}
	if machine.Spec.Version != nil {
		result["kube_version"] = *machine.Spec.Version
		result["hyperkube_image_tag"] = *machine.Spec.Version
	} else {
		result["kube_version"] = ""
		result["hyperkube_image_tag"] = ""
	}
	return result
}

func defaultFixedConfig() map[string]interface{} {
	return map[string]interface{}{
		"yum_repo_ip":                      "",
		"registry_ip":                      "",
		"etcd_data_dir":                    "/etcd",
		"data_dir":                         "/etcd",
		"containerd_lib_path":              "/runtime",
		"kubelet_root":                     "/kubelet",
		"ecms_domain_custom_enabled":       true,
		"harbor_port":                      9443,
		"harbor_core_replicas":             1,
		"harbor_registry_replicas":         1,
		"cloud_provider":                   "external",
		"psbc_log_dump_enable":             true,
		"upstream_nameservers":             "",
		"webhook_enabled":                  true,
		"charts_repo_ip":                   "10.222.255.253",
		"helm_enabled":                     true,
		"dnscache_enabled":                 true,
		"kubepods_reserve":                 true,
		"fs_server":                        "10.20.0.2",
		"fs_server_ip":                     "",
		"epel_enabled":                     false,
		"docker_repo_enabled":              false,
		"kubeadm_enabled":                  false,
		"populate_inventory_to_hosts_file": false,
		"preinstall_selinux_state":         "disabled",
		"container_lvm_enabled":            false,
		"nvidia_driver_install_container":  false,
		"prometheus_operator_enabled":      true,
		"grafana_enabled":                  true,
		"repo_prefix":                      "",
		"registry_prefix":                  "",
		"registry_admin_name":              "",
		"registry_admin_password":          "",
	}
}

func defaultBusinessConfig(scope *Scope) map[string]interface{} {
	result := map[string]interface{}{}
	if scope != nil && scope.Cluster != nil && scope.Cluster.Spec.ClusterNetwork != nil {
		cn := scope.Cluster.Spec.ClusterNetwork
		if len(cn.Services.CIDRBlocks) > 0 {
			result["kube_service_addresses"] = cn.Services.CIDRBlocks[0]
		}
		if len(cn.Pods.CIDRBlocks) > 0 {
			result["kube_pods_subnet"] = cn.Pods.CIDRBlocks[0]
		}
	}
	return result
}

func mergeSectionMaps(base, overrides map[string]interface{}) map[string]interface{} {
	if len(overrides) == 0 {
		return base
	}
	result := make(map[string]interface{}, len(base)+len(overrides))
	for k, v := range base {
		result[k] = v
	}
	for k, v := range overrides {
		result[k] = v
	}
	return result
}

func (r *AnsibleConfigReconciler) fetchObjectForRef(ctx context.Context, ref *corev1.ObjectReference, defaultNamespace string) (*unstructured.Unstructured, error) {
	if ref == nil || ref.APIVersion == "" || ref.Kind == "" || ref.Name == "" {
		return nil, nil
	}
	gv, err := schema.ParseGroupVersion(ref.APIVersion)
	if err != nil {
		return nil, err
	}
	gvk := gv.WithKind(ref.Kind)
	mapping, err := r.RESTMapper.RESTMapping(gvk.GroupKind(), gvk.Version)
	if err != nil {
		return nil, err
	}
	resource := r.DynamicClient.Resource(mapping.Resource)
	if mapping.Scope.Name() == meta.RESTScopeNameNamespace {
		ns := ref.Namespace
		if ns == "" {
			ns = defaultNamespace
		}
		return resource.Namespace(ns).Get(ctx, ref.Name, metav1.GetOptions{})
	}
	return resource.Get(ctx, ref.Name, metav1.GetOptions{})
}

func assignString(target map[string]interface{}, obj map[string]interface{}, key string, path ...string) {
	if value, found, _ := unstructured.NestedString(obj, path...); found && value != "" {
		target[key] = value
	}
}

func assignBool(target map[string]interface{}, obj map[string]interface{}, key string, path ...string) {
	if value, found, _ := unstructured.NestedBool(obj, path...); found {
		target[key] = value
	}
}

func assignStringSlice(target map[string]interface{}, obj map[string]interface{}, key string, path ...string) {
	values, found, _ := unstructured.NestedSlice(obj, path...)
	if !found || len(values) == 0 {
		return
	}
	formatted := make([]string, 0, len(values))
	for _, v := range values {
		if str, ok := v.(string); ok && str != "" {
			formatted = append(formatted, str)
		}
	}
	if len(formatted) > 0 {
		target[key] = formatted
	}
}

func applyStringMappings(target map[string]interface{}, obj map[string]interface{}, mappings []fieldMapping) {
	for _, mapping := range mappings {
		assignString(target, obj, mapping.key, mapping.path...)
	}
}

func applyBoolMappings(target map[string]interface{}, obj map[string]interface{}, mappings []fieldMapping) {
	for _, mapping := range mappings {
		assignBool(target, obj, mapping.key, mapping.path...)
	}
}

func applySliceMappings(target map[string]interface{}, obj map[string]interface{}, mappings []fieldMapping) {
	for _, mapping := range mappings {
		assignStringSlice(target, obj, mapping.key, mapping.path...)
	}
}

func (r *AnsibleConfigReconciler) assignOpenStackSecret(ctx context.Context, scope *Scope, infraObj *unstructured.Unstructured, dest map[string]interface{}) error {
	if infraObj == nil || r.SecretCachingClient == nil {
		return nil
	}
	refMap, found, _ := unstructured.NestedMap(infraObj.Object, "spec", "credentials", "openStack", "secretRef")
	if !found {
		return nil
	}
	name, _ := refMap["name"].(string)
	if name == "" {
		return nil
	}
	ns, _ := refMap["namespace"].(string)
	if ns == "" {
		ns = scope.Cluster.Namespace
	}
	key := "password"
	if k, ok := refMap["key"].(string); ok && k != "" {
		key = k
	}
	secret := &corev1.Secret{}
	if err := r.SecretCachingClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, secret); err != nil {
		return err
	}
	if data, ok := secret.Data[key]; ok {
		dest["openstack_password"] = string(data)
	}
	return nil
}

func normalizeConfigData(raw interface{}) map[string]interface{} {
	switch typed := raw.(type) {
	case map[string]interface{}:
		return typed
	case map[string]string:
		out := make(map[string]interface{}, len(typed))
		for k, v := range typed {
			var decoded interface{}
			if err := yaml.Unmarshal([]byte(v), &decoded); err == nil {
				out[k] = decoded
			} else {
				out[k] = v
			}
		}
		return out
	case string:
		out := map[string]interface{}{}
		if err := yaml.Unmarshal([]byte(typed), &out); err == nil {
			return out
		}
		return map[string]interface{}{"value": typed}
	default:
		return map[string]interface{}{}
	}
}

func cloneMap(in map[string]interface{}) map[string]interface{} {
	out := make(map[string]interface{}, len(in))
	for k, v := range in {
		switch typed := v.(type) {
		case map[string]interface{}:
			out[k] = cloneMap(typed)
		default:
			out[k] = typed
		}
	}
	return out
}

func (r *AnsibleConfigReconciler) aggregateNodeResources(ctx context.Context, scope *Scope, host *clusterv1.Machine) (map[string]interface{}, error) {
	nodes := map[string]*clusterv1.Machine{}
	addMachine := func(machine *clusterv1.Machine) {
		if machine == nil || machine.Name == "" {
			return
		}
		nodes[machine.Name] = machine
	}
	addMachine(host)

	status, err := r.lookupControlPlaneStatus(ctx, scope)
	if err != nil {
		return nil, err
	}
	if status != nil {
		if status.ControlPlaneInitialMachine != nil {
			cpMachine, err := machineFromReference(ctx, r.Client, scope, status.ControlPlaneInitialMachine)
			if err != nil {
				return nil, err
			}
			addMachine(cpMachine)
		}
		for i := range status.EtcdInitialMachines {
			ref := status.EtcdInitialMachines[i]
			etcdMachine, err := machineFromReference(ctx, r.Client, scope, &ref)
			if err != nil {
				return nil, err
			}
			addMachine(etcdMachine)
		}
	}

	nodeResources := map[string]interface{}{}
	for name, machine := range nodes {
		values, err := r.nodeResourcesFromMachine(ctx, machine)
		if err != nil {
			return nil, err
		}
		if len(values) > 0 {
			nodeResources[name] = values
		}
	}
	if len(nodeResources) == 0 {
		return nil, nil
	}
	return nodeResources, nil
}

func (r *AnsibleConfigReconciler) nodeResourcesFromMachine(ctx context.Context, machine *clusterv1.Machine) (map[string]interface{}, error) {
	if machine == nil || machine.Spec.InfrastructureRef.APIVersion == "" {
		return nil, nil
	}
	ref := machine.Spec.InfrastructureRef
	infraMachine, err := r.fetchObjectForRef(ctx, &ref, machine.Namespace)
	if err != nil {
		return nil, err
	}
	if infraMachine == nil {
		return nil, nil
	}
	if nodeRes, found, _ := unstructured.NestedMap(infraMachine.Object, machineInfraNodeResourcesPath...); found && len(nodeRes) > 0 {
		return cloneMap(nodeRes), nil
	}
	return nil, nil
}

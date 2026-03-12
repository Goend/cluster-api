package controllers

import (
	"context"
	"fmt"
	"strconv"

	"github.com/pkg/errors"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/yaml"

	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	bootstrapv1 "sigs.k8s.io/cluster-api/bootstrap/ansible/api/v1alpha1"
	"sigs.k8s.io/cluster-api/controllers/external"
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
	// Flattened security group id (first id) exposed at top-level.
	securityGroupsValueKey = "__cilium_sg_ids"

	defaultVarsTemplate = `
  cluster_name: {{ eval "__cluster_name" }}
  ansible_config: {{ eval "__ansible_config" }}
  machine_name: {{ eval "__machine_name" }}
  ansible_user: root
  ansible_ssh_private_key_file: /auth/ssh-privatekey
  capi_feature_gates:
    KubeletProviderID: true
    LBService: true
  cilium_openstack_security_group_ids: {{ eval "__cilium_sg_ids" }}
{{- if or (eval "__bastion_fip") (eval "__cp_host") }}
  supplementary_addresses_in_ssl_keys:
{{- if (eval "__bastion_fip") }}
    - {{ eval "__bastion_fip" }}
{{- end }}
{{- if (eval "__cp_host") }}
    - {{ eval "__cp_host" }}
{{- end }}
{{- end }}
{{- /* OpenStack LB vars resolved from infra OpenStackCluster via template resource() */}}
{{- $infraName := eval "__infra_name" }}
{{- if $infraName }}
{{- $osc := eval (printf "resource('infrastructure.cluster.x-k8s.io','v1beta1','openstackclusters','%s')" $infraName) }}
{{- $fnet := index $osc "status" "externalNetwork" "id" }}
{{- if $fnet }}
  cloud_provider_openstack_lb_floating_network_id: {{ $fnet }}
{{- end }}
{{- $subnets := index $osc "status" "network" "subnets" }}
{{- if $subnets }}
{{- $first := index $subnets 0 }}
{{- $sid := index $first "id" }}
{{- if $sid }}
  cloud_provider_openstack_lb_subnet_id: {{ $sid }}
{{- end }}
{{- end }}
{{- end }}
{{ indent 2 (toYAML (eval "__merged_section")) }}
{{- if (eval "__node_resources") }}
  node_resources:
{{- range $name, $res := (eval "__node_resources") }}
    {{$name}}: {memory: {{ index $res "memory" }}}
{{- end }}
{{- end }}
`
)

type fieldMapping struct {
	key  string
	path []string
}

var (
	clusterInfraStringFields = []fieldMapping{
		{"kube_network_plugin", []string{"spec", "extensions", "networking", "kubeNetworkPlugin"}},
		{"cilium_openstack_project_id", []string{"status", "extensions", "networking", "cilium", "projectID"}},
		{"cilium_openstack_default_subnet_id", []string{"status", "extensions", "networking", "cilium", "defaultSubnetID"}},
		{"master_virtual_vip", []string{"status", "extensions", "loadBalancers", "controlPlane", "vip"}},
		{"ingress_virtual_vip", []string{"status", "extensions", "loadBalancers", "ingress", "vip"}},
		{"harbor_addr", []string{"status", "extensions", "loadBalancers", "ingress", "vip"}},
		{"cloud_master_vip", []string{"status", "extensions", "openStack", "mgmt"}},
		{"openstack_auth_domain", []string{"status", "extensions", "endpoints", "keystone"}},
		{"openstack_cinder_domain", []string{"status", "extensions", "endpoints", "cinder"}},
		{"openstack_nova_domain", []string{"status", "extensions", "endpoints", "nova"}},
		{"openstack_neutron_domain", []string{"status", "extensions", "endpoints", "neutron"}},
		{"openstack_project_name", []string{"status", "extensions", "openStack", "project"}},
		{"openstack_project_domain_name", []string{"status", "extensions", "openStack", "projectDomain"}},
		{"openstack_region_name", []string{"status", "extensions", "openStack", "region"}},
		{"ntp_server", []string{"status", "extensions", "platform", "ntp", "server"}},
		{"vip_mgmt", []string{"status", "extensions", "platform", "management", "vip"}},
		{"flannel_interface", []string{"spec", "extensions", "networkInterfaces", "flannel"}},
	}
	clusterInfraBoolFields = []fieldMapping{
		{"vpc_cni_webhook_enable", []string{"status", "extensions", "networking", "cilium", "webhookEnable"}},
	}
	clusterInfraSliceFields = []fieldMapping{
		{"cilium_openstack_security_group_ids", []string{"status", "extensions", "networking", "cilium", "securityGroupIDs"}},
	}
	machineKeepalivedSpecPath = []string{"spec", "extensions", "networkInterfaces", "keepalived"}
	configGVR                 = schema.GroupVersionResource{
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

	// Derive flattened security group id: take the first element of the array.
	var sgID string
	if v, ok := infraSection["cilium_openstack_security_group_ids"]; ok {
		if arr, ok := v.([]interface{}); ok && len(arr) > 0 {
			if s, ok := arr[0].(string); ok {
				sgID = s
			}
		} else if ss, ok := v.([]string); ok && len(ss) > 0 {
			sgID = ss[0]
		}
		// 避免在合并扁平 map 时再次输出数组形式导致重复键，移除此键。
		delete(infraSection, "cilium_openstack_security_group_ids")
	}

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
	renderer.SetValue(securityGroupsValueKey, sgID)
	// 注入补充证书地址渲染所需的变量：跳板机 FIP 与 CP 主机名。
	if fip, err := r.bastionFIP(ctx, scope); err == nil {
		renderer.SetValue("__bastion_fip", fip)
	} else {
		renderer.SetValue("__bastion_fip", "")
	}
	if scope.Cluster != nil {
		renderer.SetValue("__cp_host", scope.Cluster.Spec.ControlPlaneEndpoint.Host)
		if scope.Cluster.Spec.InfrastructureRef.IsDefined() {
			renderer.SetValue("__infra_name", scope.Cluster.Spec.InfrastructureRef.Name)
		} else {
			renderer.SetValue("__infra_name", "")
		}
	} else {
		renderer.SetValue("__cp_host", "")
		renderer.SetValue("__infra_name", "")
	}

	// Merge all section maps into a flat map for group_vars.yml top-level keys.
	merged := map[string]interface{}{}
	merged = mergeSectionMaps(merged, machineSection)
	merged = mergeSectionMaps(merged, infraSection)
	merged = mergeSectionMaps(merged, businessSection)
	merged = mergeSectionMaps(merged, fixedSection)

	// 当执行扩容（scale.yml）且为融合架构（节点同时承担 kube-master 与 etcd 角色）时，传入 scale_master: "true"
	isScale := scope.ClusterOperationPlan.ActionType == bootstrapv1.ClusterOperationActionScale
	if isScale && hasMasterRole(scope.Config.Spec.Role) && hasEtcdRole(scope.Config.Spec.Role) {
		merged["scale_master"] = "true"
	}
	if nr, ok := merged["node_resources"]; ok {
		renderer.SetValue("__node_resources", nr)
		delete(merged, "node_resources")
	} else {
		renderer.SetValue("__node_resources", nil)
	}
	renderer.SetValue("__merged_section", merged)

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

	if scope.Cluster.Spec.InfrastructureRef.IsDefined() {
		infraObj, err := external.GetObjectFromContractVersionedRef(ctx, r.Client, scope.Cluster.Spec.InfrastructureRef, scope.Cluster.Namespace)
		if err != nil {
			return nil, err
		}
		if infraObj != nil {
			obj := infraObj.UnstructuredContent()
			applyStringMappings(result, obj, clusterInfraStringFields)
			applyBoolMappings(result, obj, clusterInfraBoolFields)
			applySliceMappings(result, obj, clusterInfraSliceFields)
		}
	}

	nodeResources, err := r.aggregateNodeResources(ctx, scope, machine)
	if err != nil {
		return nil, err
	}
	if len(nodeResources) > 0 {
		result["node_resources"] = nodeResources
	}

	if keepalived, err := r.machineKeepalivedInterface(ctx, machine); err != nil {
		return nil, err
	} else if keepalived != "" {
		result["keepalived_interface"] = keepalived
	}

	// 覆盖/赋值 vip_mgmt：将集群的 ControlPlaneEndpoint.Host 直接传递给 ABP 变量。
	// 这样可以确保管理面 VIP 与 Cluster-Level 的端点主机名保持一致。
	if scope.Cluster != nil {
		if host := scope.Cluster.Spec.ControlPlaneEndpoint.Host; host != "" {
			result["vip_mgmt"] = host
		}
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
	version := machine.Spec.Version
	if version == "" {
		result["kube_version"] = ""
		result["hyperkube_image_tag"] = ""
		return result
	}
	result["kube_version"] = version
	result["hyperkube_image_tag"] = version
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
	if scope == nil || scope.Cluster == nil {
		return result
	}
	cn := scope.Cluster.Spec.ClusterNetwork
	if len(cn.Services.CIDRBlocks) > 0 {
		result["kube_service_addresses"] = cn.Services.CIDRBlocks[0]
	}
	if len(cn.Pods.CIDRBlocks) > 0 {
		result["kube_pods_subnet"] = cn.Pods.CIDRBlocks[0]
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

	primaryMachine, etcdMachines, err := r.findAnchorMachines(ctx, scope)
	if err != nil {
		return nil, err
	}
	addMachine(primaryMachine)
	for _, machine := range etcdMachines {
		addMachine(machine)
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
	if machine == nil || !machine.Spec.InfrastructureRef.IsDefined() {
		return nil, nil
	}
	infraMachine, err := external.GetObjectFromContractVersionedRef(ctx, r.Client, machine.Spec.InfrastructureRef, machine.Namespace)
	if err != nil {
		return nil, err
	}
	if infraMachine == nil {
		return nil, nil
	}
	// 对接 CAPO：读取 spec.extensions.memory.reserved，生成 node_resources.
	if valStr, found, _ := unstructured.NestedString(infraMachine.Object, "spec", "extensions", "memory", "reserved"); found && valStr != "" {
		var memVal interface{} = valStr
		if i, err := strconv.Atoi(valStr); err == nil {
			memVal = i
		}
		return map[string]interface{}{"memory": memVal}, nil
	}
	return nil, nil
}

func (r *AnsibleConfigReconciler) machineKeepalivedInterface(ctx context.Context, machine *clusterv1.Machine) (string, error) {
	if machine == nil || !machine.Spec.InfrastructureRef.IsDefined() {
		return "", nil
	}
	infraMachine, err := external.GetObjectFromContractVersionedRef(ctx, r.Client, machine.Spec.InfrastructureRef, machine.Namespace)
	if err != nil {
		return "", err
	}
	if infraMachine == nil {
		return "", nil
	}
	if value, found, _ := unstructured.NestedString(infraMachine.Object, machineKeepalivedSpecPath...); found && value != "" {
		return value, nil
	}
	return "", nil
}

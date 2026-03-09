/*
Copyright 2026 The Kubernetes Authors.

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

package controllers

import (
	"context"
	"fmt"

	"github.com/pkg/errors"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	ansiblecloudinit "sigs.k8s.io/cluster-api/bootstrap/ansible/internal/cloudinit"
	"sigs.k8s.io/cluster-api/controllers/external"
	"sigs.k8s.io/cluster-api/util"
	client "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"
)

// 注意：本包使用 sigs.k8s.io/yaml 进行反序列化；该库会先将 YAML 转为 JSON 再用 encoding/json 反序列化，
// 因此这里必须使用 json 标签而不是 yaml 标签，避免字段解析失败导致必填字段判空。
type openStackCloudsFile struct {
	Clouds map[string]openStackCloudEntry `json:"clouds"`
}

type openStackCloudEntry struct {
	Auth       openStackCloudAuth `json:"auth"`
	RegionName string             `json:"region_name"`
}

type openStackCloudAuth struct {
	AuthURL             string `json:"auth_url"`
	AppCredentialID     string `json:"application_credential_id"`
	AppCredentialSecret string `json:"application_credential_secret"`
}

func (r *AnsibleConfigReconciler) buildOpenStackAppCredentialFiles(ctx context.Context, scope *Scope) ([]ansiblecloudinit.File, error) {
	if scope == nil || scope.Cluster == nil {
		return nil, nil
	}
	machine, err := machineFromScope(scope)
	if err != nil {
		return nil, err
	}
	if machine == nil || !util.IsControlPlaneMachine(machine) {
		return nil, nil
	}
	if !scope.Cluster.Spec.InfrastructureRef.IsDefined() {
		return nil, nil
	}
	infraObj, err := external.GetObjectFromContractVersionedRef(ctx, r.Client, scope.Cluster.Spec.InfrastructureRef, scope.Cluster.Namespace)
	if err != nil {
		return nil, err
	}
	if infraObj == nil {
		return nil, nil
	}
	ref, found, err := unstructured.NestedString(infraObj.UnstructuredContent(), "status", "extensions", "openStack", "appCredential", "ref")
	if err != nil {
		return nil, err
	}
	if !found || ref == "" {
		return nil, nil
	}
	secretClient := r.SecretCachingClient
	if secretClient == nil {
		secretClient = r.Client
	}
	secret := &corev1.Secret{}
	namespace := infraObj.GetNamespace()
	if namespace == "" {
		namespace = scope.Cluster.Namespace
	}
	if err := secretClient.Get(ctx, types.NamespacedName{Namespace: namespace, Name: ref}, secret); err != nil {
		return nil, err
	}
	cloudsData, ok := secret.Data["clouds.yaml"]
	if !ok {
		return nil, errors.Errorf("app credential secret %s/%s missing clouds.yaml", namespace, ref)
	}
	cloudName := fmt.Sprintf("%s-%s", scope.Cluster.Namespace, scope.Cluster.Name)
	entry, err := parseOpenStackCloudEntry(cloudsData, cloudName, scope.Cluster.Name)
	if err != nil {
		return nil, err
	}
	if entry.Auth.AuthURL == "" || entry.Auth.AppCredentialID == "" || entry.Auth.AppCredentialSecret == "" || entry.RegionName == "" {
		return nil, errors.New("app credential secret is missing required auth fields")
	}
	authOpts := fmt.Sprintf("auth-url=%s\napplication-credential-id=%s\napplication-credential-secret=%s\nregion=%s\n", entry.Auth.AuthURL, entry.Auth.AppCredentialID, entry.Auth.AppCredentialSecret, entry.RegionName)
	cloudConfig := fmt.Sprintf("[Global]\nauth-url=%s\napplication-credential-id=%s\napplication-credential-secret=%s\nregion=%s\n[BlockStorage]\nbs-version=v2\nignore-volume-az=True\n", entry.Auth.AuthURL, entry.Auth.AppCredentialID, entry.Auth.AppCredentialSecret, entry.RegionName)
	files := []ansiblecloudinit.File{
		{
			Path:        "/opt/auth-opts",
			Owner:       defaultBootstrapFileOwner,
			Permissions: "0600",
			Content:     authOpts,
		},
		{
			Path:        "/opt/cloud_config",
			Owner:       defaultBootstrapFileOwner,
			Permissions: "0600",
			Content:     cloudConfig,
		},
	}
	return files, nil
}

func parseOpenStackCloudEntry(data []byte, primaryName string, fallbackName string) (openStackCloudEntry, error) {
	var clouds openStackCloudsFile
	if err := yaml.Unmarshal(data, &clouds); err != nil {
		return openStackCloudEntry{}, errors.Wrap(err, "failed to parse clouds.yaml")
	}
	if len(clouds.Clouds) == 0 {
		return openStackCloudEntry{}, errors.New("clouds.yaml contains no clouds entries")
	}
	if primaryName != "" {
		if entry, ok := clouds.Clouds[primaryName]; ok {
			return entry, nil
		}
	}
	if fallbackName != "" {
		if entry, ok := clouds.Clouds[fallbackName]; ok {
			return entry, nil
		}
	}
	if len(clouds.Clouds) == 1 {
		for _, entry := range clouds.Clouds {
			return entry, nil
		}
	}
	if primaryName != "" {
		return openStackCloudEntry{}, errors.Errorf("clouds.yaml missing cloud %s", primaryName)
	}
	return openStackCloudEntry{}, errors.New("clouds.yaml missing expected cloud entry")
}

// firstOpenStackClusterCIDR 收集 OpenStackCluster 在 status.network.subnets 中暴露的 CIDRs，
// 返回找到的第一个非空 CIDR 字符串；未找到或非 OpenStack 基础设施则返回空串。
func (r *AnsibleConfigReconciler) firstOpenStackClusterCIDR(ctx context.Context, scope *Scope) (string, error) {
	if scope == nil || scope.Cluster == nil || !scope.Cluster.Spec.InfrastructureRef.IsDefined() {
		return "", nil
	}
	infraObj, err := external.GetObjectFromContractVersionedRef(ctx, r.Client, scope.Cluster.Spec.InfrastructureRef, scope.Cluster.Namespace)
	if err != nil {
		return "", err
	}
	if infraObj == nil {
		return "", nil
	}
	// 读取 status.network.subnets[*].cidr
	obj := infraObj.UnstructuredContent()
	subnets, found, err := unstructured.NestedSlice(obj, "status", "network", "subnets")
	if err != nil {
		return "", err
	}
	if !found || len(subnets) == 0 {
		return "", nil
	}
	for _, s := range subnets {
		if m, ok := s.(map[string]interface{}); ok {
			if cidr, foundCidr, _ := unstructured.NestedString(m, "cidr"); foundCidr && cidr != "" {
				return cidr, nil
			}
		}
	}
	return "", nil
}

// firstOpenStackClusterCIDRFromClient 是 firstOpenStackClusterCIDR 的无接收者版本，
// 方便在无 AnsibleConfigReconciler 实例的工具函数中复用。
func firstOpenStackClusterCIDRFromClient(ctx context.Context, c client.Client, scope *Scope) (string, error) {
	if scope == nil || scope.Cluster == nil || !scope.Cluster.Spec.InfrastructureRef.IsDefined() {
		return "", nil
	}
	infraObj, err := external.GetObjectFromContractVersionedRef(ctx, c, scope.Cluster.Spec.InfrastructureRef, scope.Cluster.Namespace)
	if err != nil {
		return "", err
	}
	if infraObj == nil {
		return "", nil
	}
	obj := infraObj.UnstructuredContent()
	subnets, found, err := unstructured.NestedSlice(obj, "status", "network", "subnets")
	if err != nil {
		return "", err
	}
	if !found || len(subnets) == 0 {
		return "", nil
	}
	for _, s := range subnets {
		if m, ok := s.(map[string]interface{}); ok {
			if cidr, foundCidr, _ := unstructured.NestedString(m, "cidr"); foundCidr && cidr != "" {
				return cidr, nil
			}
		}
	}
	return "", nil
}

---
title: Vars Rendering Pipeline for ACP/KCP Interop
authors:
  - "@codex-bot"
reviewers:
  - "@jian"
  - "@sig-cluster-lifecycle"
creation-date: 2026-02-05
status: provisional
---

# Vars Rendering Pipeline（ACP 通用设计草案）

## 背景

- ACP（AnsibleControlPlane）需要将大量 `vars.yaml` 字段拼装给 Kubean/Kubespray；这些字段来源散落在 Machine、Infra provider、业务配置乃至系统常量中，当前写死在控制器里，难以演进 所以进行一次依赖反转。
- KCP/CABPK 在 kubeadm 模式下也需要类似能力（InitConfiguration/ClusterConfiguration patch），因此希望抽象出一套“数据 CRD + 模板渲染”机制，供不同控制面复用。

## 目标

1. **集中式数据源**：通过一个“纯数据” Config CR（参考 `configs.servicecatalog.ecp.com`）承载 vars；控制器只需消费 CR，不再硬编码字段。
2. **规则驱动取值**：在 Config CR 中定义字段如何从其它资源派生（如 `$(machine.<name>.spec.version)`）；渲染器负责解析规则并填充默认值。
3. **跨 Provider 复用**：ACP/KCP 都可以引用同一套规则/模板，只需切换数据源（CAPI Machine、Infra Status、业务 Config）。
4. **安全/幂等**：限定可访问的资源类型与命名空间，支持缓存和版本检测，避免重复拉取 API。

## 范式划分（同步 bootstrap/ansible README）

ansible bootstrap 的 README（`bootstrap/ansible/README.md`）已经沉淀出一套经过实践验证的变量分类。为了保持控制面与 bootstrap 两侧一致，本提案将照搬该结构，并明确渲染器应如何消费。

### 1. Machine / ControlPlane 推导类

| 参数/字段 | 数据来源 | 说明 |
| --- | --- | --- |
| `kube_version` | `Machine.spec.version`（若为空则回落到 `ACP.spec.version`） | Machine 自带的 Kubernetes 版本声明，确保扩容/修复一致。 |
| `hyperkube_image_tag` | 同 `kube_version` | 直接由 `kube_version` 派生，避免重复输入。 |
| Inventory 分组（`[kube-master]`、`[etcd]` 等） | `AnsibleConfig.spec.role` | 控制器根据角色数组渲染 inventory，每个角色自动落位。 |

> 这些字段完全由 CAPI 对象决定，渲染器只负责读取和格式化，不允许通过 Config CR 覆盖。

### 2. 基础设施 Provider 调谐类

| 参数/字段 | 示例数据源（最终以 provider 实现为准） | 说明 |
| --- | --- | --- |
| `kube_network_plugin` | `InfraCluster.status.network.plugin` | Provider 选择的 CNI 名称。 |
| `cilium_openstack_project_id` | `InfraCluster.status.network.openstack.projectID` | Cilium VPC-CNI 的项目 ID。 |
| `cilium_openstack_default_subnet_id` | `InfraCluster.status.network.openstack.subnets['cilium-default']` | Pod 默认子网。 |
| `cilium_openstack_security_group_ids` | `InfraCluster.status.network.openstack.securityGroups` | Pod/节点安全组。 |
| `vpc_cni_webhook_enable` | `InfraCluster.spec.network.features.vpcniWebhook` | 是否开启 Captain 的 VPC-CNI webhook。 |
| `master_virtual_vip` | `InfraCluster.status.loadBalancer.controlPlane.vip` | 控制面 Keepalived VIP。 |
| `ingress_virtual_vip` | `InfraCluster.status.loadBalancer.ingress.vip` 或 `InfraMachine.status.loadBalancer.ingress.vip` | Ingress/Harbor 所需 VIP。 |
| `keepalived_interface` | `InfraMachine.status.network.interface.default` | 控制面节点默认网卡。 |
| `harbor_addr` | 同上 | Harbor 对外地址，通常复用 ingress VIP。 |
| `cloud_master_vip` | `InfraCluster.status.platform.controlPlaneVIP` | IaaS 管理/公网 VIP。 |
| `openstack_auth_domain`/`openstack_cinder_domain`/`openstack_nova_domain`/`openstack_neutron_domain` | `InfraCluster.status.platform.openstack.*Endpoint` | OpenStack 各服务地址。 |
| `openstack_user_name`/`openstack_password` | `InfraCluster.spec.credentials.openstack`（密码通过 Secret 引用） | 平台统一账号。 |
| `openstack_project_name`/`openstack_project_domain_name` | `InfraCluster.status.platform.openstack` | 集群所属项目/域。 |
| `openstack_user_app_cred_name` | `InfraCluster.status.platform.openstack.applicationCredentialName` | App Credential 名称。 |
| `openstack_region_name` | `InfraCluster.status.platform.openstack.region` | 部署区域。 |
| `ntp_server` | `InfraCluster.status.network.timeServer` | 平台 NTP。 |
| `vip_mgmt` | `InfraCluster.status.loadBalancer.management.vip` | 管理网 VIP。 |
| `flannel_interface` | `InfraCluster.status.network.interface.flannel` | Flannel/VPC-CNI 对应宿主机网卡。 |
| `node_resources.<machine>` | `InfraMachine.spec.resources.memory.reserved` | 宿主机预留资源；首次支持即来自基础设施，而非手工输入。 |

> 渲染器会先尝试直接读取 provider 写入的字段，然后再与 `<cluster>-vars-infra` Config CR 合并。缺失字段只会记录告警，不做推断。

### 3. 业务配置类（一次定义，多处复用）

| 参数/字段 | 建议来源 | 说明 |
| --- | --- | --- |
| `kube_service_addresses` | 网络配置 CR（如 `ClusterNetworkConfig`） | Service CIDR。 |
| `ecms_domain_custom` / `ecms_domain_custom_enabled` | 监控/平台 Config | ECMS/Prometheus 域名及开关。 |
| `prometheus_retention_time` / `prometheus_pv_size` / `thanos_ruler_pv_size` | 监控 Config | 监控持久化配置。 |
| `harbor_domain` / `harbor_admin_password` / `registry_pvc_size` / `chartmuseum_pvc_size` / `jobservice_pvc_size` / `database_pvc_size` / `redis_pvc_size` | Harbor Config | Harbor 域名、凭据与容量。 |
| `volume_type` | 存储/平台 Config | 云盘或后端存储类型。 |
| `nvidia_accelerator_enabled` / `hygon_accelerator_enabled` | GPU/异构 Config | 是否启用 GPU/DCU。 |
| `webhook_enabled` | Keystone/Auth Config | Keystone-auth 等 webhook 控制。 |
| `kube_pods_subnet` | 网络 Config | Pod CIDR。 |

> 这类变量统一由 `<cluster>-vars-business` Config CR 管理，渲染器按“Provider 默认 < 业务覆盖”的优先级合并。

### 4. 固定配置类（系统常量）

| 参数/字段 | 默认值 / 来源 | 说明 |
| --- | --- | --- |
| `yum_repo_ip` / `registry_ip` | 平台预置 | 软件/镜像仓库 IP。 |
| `etcd_data_dir` / `data_dir` | `/etcd` | 满足邮储基线的 etcd 数据路径。 |
| `containerd_lib_path` / `kubelet_root` | `/runtime` / `/kubelet` | 容器运行时与 kubelet 目录。 |
| `harbor_port` / `harbor_core_replicas` / `harbor_registry_replicas` | `9443` / `1` / `1` | Harbor 固定参数。 |
| `cloud_provider` | `external` | Captain 模式固定值。 |
| `psbc_log_dump_enable` | `true` | 邮储默认开启日志重定向。 |
| `upstream_nameservers` | `''` | 默认空。 |
| `charts_repo_ip` / `helm_enabled` / `dnscache_enabled` / `kubepods_reserve` | 固定值（见 README） | 平台通用设置。 |
| `fs_server` / `fs_server_ip` / `vip_mgmt` | 平台预置 | 文件服务与管理 VIP。 |
| `epel_enabled` / `docker_repo_enabled` / `kubeadm_enabled` / `populate_inventory_to_hosts_file` / `preinstall_selinux_state` / `container_lvm_enabled` / `nvidia_driver_install_container` | 固定布尔 | 系统基线。 |
| `prometheus_operator_enabled` / `grafana_enabled` | `true` | 可观测性组件。 |
| `repo_prefix` / `registry_prefix` / `registry_admin_name` / `registry_admin_password` | 平台 Secret 或打包默认值 | 镜像仓库统一信息。 |

> 固定配置由平台下发或 baked-in 常量控制，渲染器只读，不提供覆盖入口。

## 方案概要

1. **CRD**：引入 `Config`（namespaced，`x-kubernetes-preserve-unknown-fields`），用于存放键值对、Secret 引用以及 `rules` 字段。
2. **规则语法**：采用 `$(<resource>.<name-or-selector>.<fieldpath> | default <value>)` 风格，渲染时解析为真实对象；允许链式 fallback 类似于helm tpl。
3. **渲染器**：
   - 读取 ConfigCR + ACP/KCP + Cluster + Machine + Infra 对象；
   - 维护 LRU 缓存，避免频繁 API 调用；
   - 提供校验（缺失字段、循环依赖、非法访问）。
4. **模板输出**：将解析结果合并到 `vars.yaml`（ACP）或 `kubeadm config`（KCP），同时保留原始 Config 以便 diff/audit。

## 与现状差异

- 现状：ACP 控制器中散落 `map[string]interface{}` 拼接；每新增字段都要改代码。
- 新方案：控制器只负责调用“VarsRenderer”接口，输入对象上下文和 ConfigCR 名称，输出最终 map；具体字段清单可由运维团队直接在 Config CR 中维护。

## 开放问题

1. 规则语法的详细定义（是否支持列表/循环、条件等）。
2. Secret 解密策略——是否允许引用跨命名空间 Secret。
3. 版本管理：Config 变更如何触发 Machine/AnsibleConfig 的重新渲染。

## 下一步

1. 在 ACP 中实现原型渲染器，先支持 key→value 映射与简单路径引用。
2. 为 Config CR 提供默认样例（覆盖目前 README 中列举的全部字段）。
3. 评估 KCP 是否需要同样机制，若需要则抽象为共享库（`pkg/vars/render`）。

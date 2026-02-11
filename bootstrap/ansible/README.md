### 摘要
众所周知 在cluster-api中 bootstrap负责将服务器转变为kubernetes节点
在 Cluster API v1alpha1 中,用户可以创建 Machine 自定义资源。当创建机器资源时,集群API组件会通过配置基础设施(通常为物理或虚拟机),然后在已配置的服务器上启动Kubernetes来进行响应,使其成为Kubernetes节点。集群API当前架构要求单个“提供者”同时处理基础设施配置和Kubernetes引导。

### 更多实现细节
https://cluster-api.sigs.k8s.io/developer/providers/contracts/bootstrap-config

### 动机
- kubeadm bootstrap当前的支持需要在虚拟机或者集群中绑定kubeadm 缺少对应的自由度 同时 由于历史遗留原因 和eos使用一套安装kubernetes平面配置(类似于kubespray 的captain项目 ansible项目)具有更好的维护性
- kubeadm目前对于升级的条件过于苛刻 使用captain则没有升级硬性限制
- bootstrap日志更清晰 ansible对比kubeadm有更清晰的日志信息

### 目标
1. 实现 ansible bootstrap控制器实现将machine转换为kubernetes节点
2. 支持不同的 kubernetes 版本
3. 配置足够简单

### 非目标/未来工作
1. other

### Data model
1.machine 与 bootstrap 相关的数据模型
- MachineSpec
```go
type MachineSpec struct{
// Bootstrap is a reference to a local struct which encapsulates
// fields to configure the Machine’s bootstrapping mechanism.
Bootstrap Bootstrap `json:"bootstrap"`

// Version defines the desired Kubernetes version.
// This field is meant to be optionally used by bootstrap providers.
// +optional
Version *string `json:"version,omitempty"`
}
```
其中 
Version 为预期的kubernetes版本 理论上即将实现的ansible bootstrap控制器的版本入口
Bootstrap 为bootstrap将注入实际的machine相关的数据
- MachineStatus
```go
type MachineStatus struct {
// BootstrapReady is the state of the bootstrap provider.
// +optional
BootstrapReady bool `json:"bootstrapReady"`
}

```
- Bootstrap
```go
type Bootstrap struct {
	// ConfigRef is a reference to a bootstrap provider-specific resource
	// that holds configuration details. The reference is optional to
	// allow users/operators to specify Bootstrap.DataSecretName without
	// the need of a controller.
	// +optional
	ConfigRef *corev1.ObjectReference `json:"configRef,omitempty"`

	// DataSecretName is the name of the secret that stores the bootstrap data script.
	// If nil, the Machine should remain in the Pending state.
	// +optional
	DataSecretName *string `json:"dataSecretName,omitempty"`
}
```
注意 
DataSecretName如果为空 Machine的状态将留在Pending 也就是在ansible bootstrap的实现中 必须保证产出此DataSecret

- AnsibleConfigStatus（契合 v1beta2 Bootstrap Contract）
```go
type AnsibleConfigStatus struct {
    Ready bool `json:"ready"`
    Initialization *BootstrapDataInitializationStatus `json:"initialization,omitempty"`
    DataSecretName *string `json:"dataSecretName,omitempty"`
    // ...
}

type BootstrapDataInitializationStatus struct {
    DataSecretCreated bool `json:"dataSecretCreated,omitempty"`
}
```

其中 `Initialization.DataSecretCreated` 一旦为 `true`，表示 bootstrap data Secret 已成功写入，可被 Machine Controller 安全引用；同时保留 `status.ready` 以兼容旧版控制面消费逻辑。

### 节点角色（`spec.role`）

Ansible Bootstrap Provider 通过 `AnsibleConfig.spec.role`（类型为 `[]string`，允许单个节点扮演多个角色以支持融合场景）来区分节点的职责，并在生成 inventory 时保持一致性：

- `spec.role`：描述当前 Machine 所扮演的任务（如 `kube-master`、`etcd`、`ingress` 等），用于在实例自身的 cloud-init 与后续 hook 中注入最小自描述；
- 同一个节点可在 `role` 中声明多个字符串，从而出现在多个 Ansible group 中（例如既是 `kube-master` 又是 `ingress`）。

在 postBootstrap 逻辑中，控制器会根据 `role` 渲染出 Kubespray 所需的 `inventory.ini`（针对当前节点），并额外拉取集群内所有 `kube-master`、`etcd` 角色节点确保扩容时能够引用这些关键成员，输出顺序按照 Machine 创建时间排序以保证 determinism：

```
[kube-master]
master-1 ansible_ssh_host=10.0.0.10
master-2 ansible_ssh_host=10.0.0.11

[etcd]
etcd-1 ansible_ssh_host=10.0.0.12

[ingress]
ingress-1 ansible_ssh_host=10.0.0.20
```

- 只要 `role` 中声明了某个组名，就会生成对应的 `[group]` 并填入该节点；
- 未声明的角色不会出现在 inventory 中，避免额外的空 group；默认仍会生成 `[kube-node]` 与 `k8s-cluster:children` 等基础分组。

这样设计带来的好处：
1. Machine 级别通过 `spec.role` 自描述自身职责，并可声明多个角色；
2. inventory 自动匹配角色名称到 Ansible group，减少人工维护；
3. Kubespray/Ansible Playbook 可以直接依赖这些 group，自动加载特定角色的变量与任务。

### Cluster API Machine 生命周期状态与转换说明

下面内容是对 **Cluster API（CAPI）中 Machine 生命周期状态机** 的整理与解释，包含每个阶段的含义、转换条件以及设计意图。

---

#### 总体说明（Big Picture）

- **只有 Machine Controller 可以写入 `Machine.Status` 和 `Machine.Status.Phase`**
- 其他控制器（Bootstrap / Infrastructure Provider）**只通过各自资源的 Status 来表达就绪状态**
- Machine Controller **负责观察这些信号并推动状态流转**

👉 Machine 更像是一个 **编排者（orchestrator）**，而不是具体执行者。

---

#### 生命周期阶段详解

##### 1. Pending（待处理）

**含义**
- Machine 对象已经创建，但生命周期尚未真正开始。

**进入条件**
- `Machine.Status.Phase == ""`
- Machine Controller 将其设置为 `pending`

**Bootstrap 行为语义（非常关键）**

| `Machine.Spec.Bootstrap.Data` | 含义 |
|---|---|
| `nil` | 需要由外部控制器来填充 |
| `""`（空字符串） | 明确表示跳过 bootstrap |
| `"..."` | bootstrap 数据已经准备好 |

该设计支持：
- 外部 bootstrap provider
- 用户直接提供 cloud-init
- 无需 bootstrap 的场景

👉 **Pending 的核心含义：等待 bootstrap 就绪**

---

##### 2. Provisioning（创建中）

**含义**
- Bootstrap 已完成
- 基础设施（VM / 实例）开始创建

**转换条件**
- `Machine.Spec.Bootstrap.ConfigRef.Status.Ready == true`
- `Machine.Spec.Bootstrap.Data != nil`

**职责划分**
- Infrastructure Provider：创建计算资源
- Machine Controller：仅观察状态变化

👉 **Provisioning = 基础设施正在创建**

---

##### 3. Provisioned（已创建）

**含义**
- 基础设施已创建完成
- 计算资源可用，但尚未被 Kubernetes 接管

**转换条件**
- `Machine.Spec.InfrastructureRef.Status.Ready == true`
- `Machine.Spec.ProviderID` 已从 Infrastructure 同步
- （可选）同步 `Status.Addresses`

**为什么 ProviderID 很重要**
- ProviderID 是以下对象之间的 **唯一关联键**：
    - Machine
    - Node
    - 云提供商

👉 **Provisioned = VM 已存在，但还不是 Kubernetes Node**

---

##### 4. Running（运行中）

**含义**
- Machine 已成为 Kubernetes 集群中的一个 Node

**转换条件**
- 找到 Node
- Node 的 `ProviderID == Machine.Spec.ProviderID`
- Node 状态为 `Ready`

**Machine Controller 的职责**
- 设置 `Machine.Status.NodeRef`

👉 **Running = 基础设施 + kubelet + 节点注册全部成功**

---

##### 5. Deleting（删除中）

**含义**
- 用户请求删除 Machine
- 清理流程正在进行

**转换条件**
- `Machine.ObjectMeta.DeletionTimestamp != nil`

**期望行为**
- Cordon 并 Drain 对应 Node
- 级联删除 bootstrap 和 infrastructure 相关资源

👉 **Deleting = 正在清理，不允许再创建新资源**

---

##### 6. Deleted（已删除）

**含义**
- 所有关联资源已清理完成
- 可由 API Server 垃圾回收

**转换条件**
- `Machine.ObjectMeta.DeletionTimestamp != nil`
- InfrastructureRef 已删除
- （可选）Bootstrap ConfigRef 已删除

**最终动作**
- 移除 Machine Finalizer

👉 **Deleted 是成功的终态（Terminal State）**

---

##### 7. Failed（失败）

**含义**
- 系统无法继续自动推进生命周期
- 需要人工介入

**转换条件**
- `Machine.ErrorReason` 和 / 或 `Machine.ErrorMessage` 被设置

**关键点**
- Failed 状态不会自动恢复
- 需要用户修复配置或环境问题

👉 **Failed 会中断正常生命周期**

---

#### 生命周期流转图（简化）

```text2
Pending
  ↓（bootstrap 就绪）
Provisioning
  ↓（基础设施就绪）
Provisioned
  ↓（Node Ready）
Running
  ↓（删除请求）
Deleting
  ↓（清理完成）
Deleted
```

### machine 如何使用AnsibleConfig来进行bootstrap
machine
```text
Machine
├─ Namespace: xyz
├─ Name: foo
├─ Spec
│  └─ Bootstrap
│     ├─ Type: ansibleconfig
│     ├─ ConfigRef
│     │  ├─ apiVersion: ansibleconfig.bootstrap.k8s.io/v1
│     │  ├─ kind: AnsibleConfig
│     │  └─ name: controlplane-0
│     └─ DataSecretName: <nil>
```
AnsibleConfig
```text
AnsibleConfig
├─ Namespace: xyz
├─ Name: controlplane-0
├─ Spec
│  ├─ ClusterRef
│  │     ├─ apiVersion: cluster.kubean.io/v1alpha1
│  │     ├─ kind: Cluster
│  │     ├─ name: controlplane-0
│  │     └─ spec
│  │           ├─ hostsConfRef
│  │           │     ├─ apiVersion: v1
│  │           │     ├─ kind: ConfigMap
│  │           │     ├─ namespace: xyz
│  │           │     └─ name: controlplane-hosts-conf
│  │           │
│  │           ├─ varsConfRef
│  │           │     ├─ apiVersion: v1
│  │           │     ├─ kind: ConfigMap
│  │           │     ├─ namespace: xyz
│  │           │     └─ name: controlplane-vars-conf
│  │           │
│  │           ├─ sshAuthRef
│  │           │     ├─ apiVersion: v1
│  │           │     ├─ kind: Secret
│  │           │     ├─ namespace: xyz
│  │           │     └─ name: controlplane-ssh-auth
│  │
│  ├─ ClusterOpsRef
│  │     ├─ apiVersion: clusterops.kubean.io/v1alpha1
│  │     ├─ kind: ClusterOperation
│  │     ├─ name: controlplane-init
│  │     └─ spec
│  │           ├─ cluster
│  │           │     ├─ apiVersion: cluster.kubean.io/v1alpha1
│  │           │     ├─ kind: Cluster
│  │           │     └─ name: controlplane-0
│  │           │
│  │           ├─ image
│  │           │     ├─ kubeanOperator: kubean/kubean-operator:v0.x.x
│  │           │     └─ kubespray: kubespray/kubespray:v2.xx.x
│  │           │
│  │           ├─ action
│  │           │     └─ type: cluster.yml
│  ├─ CertRef
│  │     ├─ apiVersion: v1
│  │     ├─ kind: Secret
│  │     ├─ name: <cluster-name>-ca
│  │     ├─ namespace: xyz
│  │     └─ data
│  │           ├─ tls.crt
│  │           ├─ tls.key
...

```
### 控制器内部运行逻辑
控制器内部大致分为两个阶段 preBootstrap和postBootstrap
1. preBootstrap 负责生成cloud init,这里的配置较为单一 主要包括将.spec.CertRef 中的ca证书配置到对应的路径(/etc/kubernetes/ssl/ca.pem,/etc/kubernetes/ssl/ca-key.pem) 将产生的cloud init secret xx 配置到
.spec.Bootstrap.DataSecretName 填充,Infrastructure才会开始制备，并准备好<cluster-name>-ca中填充tls.crt与tls.key 在controlplane部分中会使用此证书作为集群的连接 来判断是否kubernetes node ready.从代码来看 生成
<cluster-name>-ca的逻辑由于controlplane部分控制
2. postBootstrap 负责在Infrastructure制备后 machine状态变为Provisioned后，进行ansible操作 主要负责创建ClusterRef，ClusterOpsRef 对应的资源 负责部署kubernetes组件 在部署完毕后 理论上machine controller会对Workload
集群中进行访问 并确认对应的machine已经加入节点 核心是preBootstrap 注入的证书被使用并可以生成client rest config访问制备的集群

### 集群操作调度

- bootstrap 控制器使用与 KCP 相同的 init lock（基于 ConfigMap）来串行化 `cluster.yml` 的执行。只有首个成功获取锁的控制平面 `Machine` 会触发 `cluster.yml`；其他控制平面会等待锁释放后再执行扩容流程。
- 当集群已初始化或节点不是控制平面时，控制器默认生成 `scale.yml` 的 `ClusterOperation`。若扩容 master，会自动注入 `action.vars.scale_master=true`，普通节点则是 `false`，便于 kubean/kubespray 剧本区分角色。
- 如果用户确实需要覆盖默认行为，可在 `AnsibleConfig` 模板上显式设置 `bootstrap.cluster.x-k8s.io/operation-action` 和 `bootstrap.cluster.x-k8s.io/scale-master` 注解，控制器会优先生效。

### Vars 参数分类与管理原则

在 ansible bootstrap 中，`vars.yaml` 是控制面/组件的核心配置。随着不同角色、行业诉求不断增加，推荐按照“事实归属”拆分变量，并通过统一的 merge 逻辑生成 vars，避免硬编码：

1. **Machine / ControlPlane 推导类（来自 CAPI 规范）**

   | 参数/字段 | 数据来源 | 说明 |
   | --- | --- | --- |
   | `kube_version` | `Machine.Spec.Version` | 直接复用 CAPI Machine 上声明的 Kubernetes 版本字符串，保持扩容/修复一致性。 |
   | `hyperkube_image_tag` | `Machine.Spec.Version` | 由 `kube_version` 派生，确保镜像标签随 Machine 版本同步更新。 |
   | Inventory 分组（`[kube-master]`、`[etcd]` 等） | `AnsibleConfig.Spec.Role` | bootstrap controller 根据角色数组归类到 inventory group，保证同一角色扩容时自动继承。 |

2. **基础设施 Provider 调谐类**
其中映射到Provider的字段没有确定 但代表了关联资源的类型

| README 字段 | Infra Type | 建议 Extensions 路径                                         | 数据来源/写入方                                               | 消费方及用途                           |
|-------------|------------|----------------------------------------------------------|--------------------------------------------------------|----------------------------------|
| `kube_network_plugin` | Cluster | `spec.extensions.networking.kubeNetworkPlugin`           | 用户/ClusterClass                                        | BA 渲染 vars，决定使用 cilium/flannel 等 |
| `cilium_openstack_project_id` | Cluster | `status.extensions.networking.cilium.projectID`          | CAPO：根据网络/项目配置写入                                       | BA/ACP 配置 Cilium 与 OpenStack 集成  |
| `cilium_openstack_default_subnet_id` | Cluster | `status.extensions.networking.cilium.defaultSubnetID`    | CAPO                                                   | BA 提供给 VPC CNI                   |
| `cilium_openstack_security_group_ids` | Cluster | `status.extensions.networking.cilium.securityGroupIDs[]` | CAPO                                                   | BA 配置 Pod/节点安全组                  |
| `vpc_cni_webhook_enable` | Cluster | `status.extensions.networking.cilium.webhookEnable`      | CAPO 根据网络特性写入                                          | BA 控制是否部署相关 webhook              |
| `master_virtual_vip` | Cluster | `status.extensions.loadBalancers.controlPlane.vip`       | CAPO（API Server LB/VIP）                                | BA Inventory/vars                |
| `ingress_virtual_vip` | Cluster | `status.extensions.loadBalancers.ingress.vip`            | CAPO（Ingress LB）                                       | BA/Harbor                        |
| `keepalived_interface` | Machine | `spec.extensions.networkInterfaces.keepalived`           | 用户/ClusterClass 通过 Machine 模板指定                         | BA 设置 keepalived                 |
| `harbor_addr` | Cluster | `status.extensions.loadBalancers.ingress.vip`            | CAPO/用户                                                | BA 渲染 控制面harbor入口ingress vip     |
| `cloud_master_vip` | Cluster | `status.extensions.openStack.mgmt`                       | CAPO（管理网络 公网IP）                                        | BA/外部访问                          |
| `openstack_auth_domain` | Cluster | `status.extensions.openStack.keystone`                   | CAPO（从 IdentityRef 解出只读 endpoint）                      | BA vars；凭证仍通过 SecretRef          |
| `openstack_cinder_domain` | Cluster | `status.extensions.openStack.cinder`                     | CAPO                                                   | BA/存储插件                          |
| `openstack_nova_domain` | Cluster | `status.extensions.openStack.nova`                       | CAPO                                                   | BA/Cloud provider 配置             |
| `openstack_neutron_domain` | Cluster | `status.extensions.openStack.neutron`                    | CAPO                                                   | BA/网络插件                          |
| `openstack_project_name` | Cluster | `status.extensions.openStack.project`                    | CAPO                                                   | BA vars                          |
| `openstack_project_domain_name` | Cluster | `status.extensions.openStack.projectDomain`              | CAPO                                                   | BA vars                          |
| `openstack_region_name` | Cluster | `status.extensions.openStack.region`                     | CAPO                                                   | BA vars                          |
| `ntp_server` | Cluster | `status.extensions.platform.ntp.server`                  | CAPO（基础设施/跳板配置） 和bastion一致                             | BA hosts/vars                    |
| `vip_mgmt` | Cluster | `status.extensions.platform.management.vip`              | CAPO（跳板机）                                              | BA/运维脚本                          |
| `flannel_interface` | Cluster | `spec.extensions.networkInterfaces.flannel`              | 用户/ClusterClass（集群唯一配置）                             | BA vars                          |
| `node_resources.<machine>` | Machine | `status.extensions.nodeResources.reserved`               | CAPO（根据 flavor/策略）  代表此批配置预留                           | BA 生成 `node_resources` 字段        |


   - OpenStack 凭据通过 cloud-init/模板注入，不在 vars renderer 清单中。
   - 控制面节点会根据 infra cluster 的 `status.extensions.openStack.appCredential.ref` 读取 Secret（沿用 CAPO 的 `clouds.yaml`），并在 cloud-init 写入 `/opt/auth-opts` 与 `/opt/cloud_config`。
   - 实现：由 infra provider 在其 CR Status、Secret 或 ConfigMap 中暴露真实值，bootstrap controller 渲染 vars 时读取 merge，确保扩容/滚动时自动继承；敏感字段（如密码）仅以 Secret 引用形式传递。

3. **业务配置类（一次定义，多处复用）**

   | 参数/字段 | 数据来源 | 说明                                                  |
   | --- | --- |-----------------------------------------------------|
   | `kube_network_plugin` | 集群级 Config CR / ClusterClass Variables | 若需要覆盖 Provider 默认插件，可在业务层指定（与 infra 字段合并时业务为更高优先级）。 |
   | `kube_service_addresses` | 配置 CR（如 `ClusterNetworkConfig`） | Service CIDR，需全局一致，通常只定义一次。                         |
   | `ecms_domain_custom` | 监控/平台 Config CR | ECMS/Prometheus 域名，变更需同步安全策略。                       |
   | `prometheus_retention_time` | 监控 Config CR | 监控数据保留时长，影响持久化容量规划。                                 |
   | `prometheus_pv_size` | 监控 Config CR | Prometheus PVC 大小。                                  |
   | `thanos_ruler_pv_size` | 监控 Config CR | Thanos Ruler PVC 大小。                                |
   | `harbor_domain` | Harbor Config CR | Harbor Ingress 域名，仅需定义一次，扩容时继承。                     |
   | `harbor_admin_password` | Harbor Config CR | Harbor 密码，扩容时继承。                                    |
   | `registry_pvc_size` | Harbor Config CR | Harbor registry 存储大小。                               |
   | `chartmuseum_pvc_size` | Harbor Config CR | ChartMuseum 存储大小。                                   |
   | `jobservice_pvc_size` | Harbor Config CR | Jobservice 存储大小。                                    |
   | `database_pvc_size` | Harbor Config CR | Harbor DB 存储大小。                                     |
   | `redis_pvc_size` | Harbor Config CR | Harbor Redis 存储大小。                                  |
   | `volume_type` | 存储/平台 Config CR | 云硬盘/存储级别，需满足租户配额。                                   |
   | `nvidia_accelerator_enabled` | GPU Config CR 或 ClusterClass 变量 | 是否启用 NVIDIA GPU；通常由业务需求驱动。                          |
   | `hygon_accelerator_enabled` | GPU/异构 Config CR | 是否启用 Hygon DCU。                                     |
   | `webhook_enabled` | Keystone/Auth Config | 是否启用 keystone-auth 等 webhook。                       |
   | `kube_pods_subnet` | 网络 Config CR | Pod CIDR，业务层需与现有网络规划保持一致。                           |

   - 实现：通过专门 Config CR（如 `ClusterNetworkConfig`、`HarborConfig`、`MonitoringConfig`）或 `AnsibleConfig` 注解集中管理，bootstrap controller 渲染 vars 时读取并合并；可在 CR `status` 中记录 `lastApplied` 以实现继承/避免覆盖。

4. **固定配置类（系统下发，不允许覆盖）**

   | 参数/字段 | 默认值 / 来源 | 说明 |
   | --- | --- | --- |
   | `yum_repo_ip` | 平台预置 | 供节点安装基础包使用的 Yum 仓库 IP。 |
   | `registry_ip` | 平台预置 | Docker/Container registry IP，一般与 `repo_prefix` 关联。 |
   | `etcd_data_dir` | `/etcd` | 满足邮储基线的 etcd 数据目录，固定值。 |
   | `data_dir` | `/etcd` | etcd 备份目录上级，要求与 `etcd_data_dir` 相同。 |
   | `containerd_lib_path` | `/runtime` | 容器运行时根目录。 |
   | `kubelet_root` | `/kubelet` | kubelet 根目录。 |
   | `ecms_domain_custom_enabled` | `true` | 邮储要求默认开启。 |
   | `harbor_port` | `9443` | Harbor Ingress 端口，基线固定。 |
   | `harbor_core_replicas` | `1` | Harbor core 副本数，默认单副本。 |
   | `harbor_registry_replicas` | `1` | Harbor registry 副本数。 |
   | `cloud_provider` | `external` | 固定为 external cloud-provider。 |
   | `psbc_log_dump_enable` | `true` | 邮储要求开启 etcd/kubelet/runtime 日志导出。 |
   | `upstream_nameservers` | `''` | 默认留空。 |
   | `webhook_enabled` | `true` | 系统默认开启 keystoneauth。 |
   | `charts_repo_ip` | `10.222.255.253` | Charts 仓库 IP。 |
   | `helm_enabled` | `true` | 是否部署 helm 组件。 |
   | `dnscache_enabled` | `true` | 是否开启 DNS cache。 |
   | `kubepods_reserve` | `true` | 是否启用 kubepods 预留。 |
   | `fs_server` | `10.20.0.2` | 文件服务地址。 |
   | `fs_server_ip` | `''` | 默认空，按需填充。 |
   | `epel_enabled` | `false` | 是否启用 EPEL。 |
   | `docker_repo_enabled` | `false` | 是否开启 docker repo。 |
   | `kubeadm_enabled` | `false` | Captain 模式下禁用 kubeadm。 |
   | `populate_inventory_to_hosts_file` | `false` | 是否写 hosts 文件。 |
   | `preinstall_selinux_state` | `disabled` | SELinux 默认关闭。 |
   | `container_lvm_enabled` | `false` | 是否启用容器 LVM。 |
   | `nvidia_driver_install_container` | `false` | GPU 驱动容器功能未启用。 |
   | `prometheus_operator_enabled` | `true` | 默认部署 Prometheus Operator。 |
   | `grafana_enabled` | `true` | 默认部署 Grafana。 |
   | `repo_prefix` | 平台预置 | 镜像仓库前缀，由系统注入。 |
   | `registry_prefix` | 平台预置 | Registry 服务前缀。 |
   | `registry_admin_name` | 系统 Secret | Harbor admin 账号。 |
   | `registry_admin_password` | 系统 Secret | Harbor admin 密码。 |

   - 这些参数由平台或发布系统直接下发，bootstrap controller 不提供覆盖入口，仅在 vars 渲染时引用。

注意: ingress 多class（非控制面ingress暂时不纳入考虑）

通用约束：

- 使用 `mergeSpecMaps` 之类的 helper 把多来源变量叠加，优先级建议为：Machine/Infra ⇒ 业务 Config ⇒ 手工覆盖。
- 敏感信息（如 OpenStack 密码）放在 Secret 中，bootstrap controller 仅引用 Secret。
- inventory/vars 中的角色扩展通过 `AnsibleConfig.Spec.Role` 与集中 helper（`inventory_helpers.go`）维护，保证每个 role 仅在一个地方描述。
- **配置入口**：`AnsibleConfig.Spec.VarsConfigRefs` 要求用户在集群 namespace 预先创建 `<cluster-name>-vars-business` 与 `<cluster-name>-vars-fixed` 两个 Config（`controlplane.cluster.x-k8s.io/v1alpha1, Kind=Config`），控制器在渲染 `vars.yaml` 时会读取它们的 `data`，缺失任意一个都会阻断 post-bootstrap 并通过事件/Condition 提示；`<cluster-name>-vars-infra` 则默认由控制器自动创建（需要覆盖时可手动编辑）用于承载基础设施推导出的事实数据。

通过上述约定，可以在基于 Cluster API 的体系内保持：

- Machine/Infra 事实自动随着扩容变更；
- 业务配置集中统一管理；
- 不修改核心 API（仅使用注解传递额外上下文）；
- 便于不同角色/行业（如邮储）定义自己的 vars 模板并继承。

### 首控批次与 Captain 耦合

与 KCP “先 init 控制面，再按角色滚动” 不同，Ansible Captain 当前的 vars 设计在多个角色之间存在强耦合。例如 `harbor_addr` = `ingress_virtual_vip`，而 coredns 的 `Corefile` 又依赖 `harbor_addr/harbor_domain/registry_ip` 等字段。如果把 ingress/harbor/prometheus 拆成独立阶段，首批控制面就无法生成完整配置，导致 DNS、证书、日志等组件缺失。因此本实现采取以下策略：

- **同一批次节点除 `kube-node` 外全部视为“控制面角色”**（etcd、master、ingress、harbor、prometheus……），统一通过 `cluster.yml` 部署；为避免 Captain 变量跨角色依赖导致的死锁，目前已停用 init lock，所有角色通过同一批次一次性执行。
- `kube-node` 以及专职 workload 节点依旧走 `scale.yml` 流程，可自由扩缩；其他角色暂不支持以 Machine 级别弹性伸缩。
- 如果需要在后续扩展第二套 ingress/harbor，可通过 Machine 注解或专门的 Config CR 下发 `ingress_label`、`virtual_router_id` 等新参数，但首批控制面依旧负责初始化 coredns、registry 等全局配置。

等 Captain 拆分 vars 之后，可以复用上文的分类（Machine/Infra/业务 Config）将 ingress/harbor/prometheus 等角色迁出首批控制面，再恢复更细粒度的扩缩策略。

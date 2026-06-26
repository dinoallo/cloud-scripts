# check-apiserver-access-candidates

扫描 Kubernetes 工作负载中“可能访问 kube-apiserver”的静态信号。这个工具用于 Kubernetes 根 CA 轮转时生成需要滚动重启的候选清单，因为长跑的集群内客户端可能需要重启或重建 client-go transport 才会重新加载 `ca.crt`。

这个工具不能证明真实运行时流量。要确认真实访问，应使用 audit log 或网络 flow log。静态候选分为：

- `likely`：Pod 有显式 projected ServiceAccount token、引用了 kubeconfig ConfigMap/Secret，或使用 automount 的 ServiceAccount token 且该 ServiceAccount 有 RBAC 绑定。
- `possible`：Pod 有 effective automounted ServiceAccount token，但没有发现更强的静态信号。

应用按 Pod 的 ownerReference 聚合。扫描器优先使用 `controller=true` 的 ownerReference；没有 controller owner 时使用第一个 ownerReference；没有 ownerReference 时按 `Pod/<name>` 输出。

## 构建

```bash
go build -o check-apiserver-access-candidates .
```

## 使用

```bash
./check-apiserver-access-candidates
./check-apiserver-access-candidates --namespace prod
./check-apiserver-access-candidates --output json --include-pods
```

选项：

- `--kubeconfig` kubeconfig 路径；不可用时回退到 in-cluster config
- `--context` 要使用的 kubeconfig context
- `--namespace` 要扫描的 namespace；空值表示所有 namespace
- `--output` 输出格式：`csv`、`table` 或 `json`
- `--include-pods` 在 JSON 输出中包含逐 Pod 明细
- `--max-samples` table 输出中每个应用最多展示的 Pod 样例数量
- `--skip-secret-inspection` 跳过 Secret 列表和 kubeconfig 检测

## 信号

扫描器会检查：

- Pod effective `automountServiceAccountToken`
- 显式 projected `serviceAccountToken` volume
- 引用 ServiceAccount 的 RoleBinding 和 ClusterRoleBinding subject
- 含 kubeconfig 数据的 ConfigMap 和 Secret
- Pod 通过 volume、projected volume、`envFrom`、单个环境变量引用这些 kubeconfig ConfigMap 或 Secret

默认输出为 CSV，包含以下列：

```text
namespace,ownerKind,ownerName,confidence,serviceAccounts,reasons,pods
```

使用 `--output table` 可以输出更宽的可读表格。JSON 输出还会包含 `unreferencedResources`，用于展示扫描到但未被任何 Pod 引用的 kubeconfig ConfigMap 或 Secret。

## Kubernetes

在集群内以一次性 Job 运行扫描器：

```bash
kubectl apply -k deploy
kubectl logs -n check-apiserver-access-candidates job/check-apiserver-access-candidates
```

同一个部署目录也包含每天集群时间 02:00 运行的 CronJob：

```bash
kubectl get cronjob -n check-apiserver-access-candidates check-apiserver-access-candidates
```

镜像覆盖和 RBAC 说明见 [deploy/README.zh-CN.md](deploy/README.zh-CN.md)。

## RBAC

最小只读权限：

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: check-apiserver-access-candidates
rules:
  - apiGroups: [""]
    resources: ["pods", "serviceaccounts", "configmaps", "secrets"]
    verbs: ["list"]
  - apiGroups: ["rbac.authorization.k8s.io"]
    resources: ["rolebindings", "clusterrolebindings"]
    verbs: ["list"]
```

当 kubeconfig 不应读取 Secret 时使用 `--skip-secret-inspection`。没有 Secret 权限时会跳过 Secret 中的 kubeconfig 候选。

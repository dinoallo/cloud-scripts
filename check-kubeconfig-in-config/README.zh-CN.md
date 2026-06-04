# check-kubeconfig-in-config

扫描 Kubernetes ConfigMap 和 Secret 中的 kubeconfig 内容，并报告引用这些资源的应用。应用按 Pod 的 controller owner 聚合，ReplicaSet 会上归为 Deployment，Job 会上归为 CronJob。

扫描器会使用 client-go 解析 kubeconfig；当内容包含 cluster 和 context 条目时判定为 kubeconfig。检查范围包括：

- ConfigMap `data` 和 `binaryData`
- Kubernetes client 解码后的 Secret `data`
- 存放在 ConfigMap 或 Secret 中的 base64 编码 kubeconfig
- Pod 通过 volume、projected volume、`envFrom`、单个环境变量引用的 ConfigMap 和 Secret

## 构建

```bash
go build -o check-kubeconfig-in-config .
```

## 使用

```bash
./check-kubeconfig-in-config
./check-kubeconfig-in-config --namespace prod
./check-kubeconfig-in-config --context my-cluster --output json --include-pods
```

## Kubernetes

在集群内以一次性 Job 运行扫描器：

```bash
kubectl apply -k deploy
kubectl logs -n check-kubeconfig-in-config job/check-kubeconfig-in-config
```

同一个部署目录也包含每天集群时间 02:00 运行的 CronJob：

```bash
kubectl get cronjob -n check-kubeconfig-in-config check-kubeconfig-in-config
```

镜像覆盖和 RBAC 说明见 [deploy/README.zh-CN.md](deploy/README.zh-CN.md)。

选项：

- `--kubeconfig` kubeconfig 路径；不可用时回退到 in-cluster config
- `--context` 要使用的 kubeconfig context
- `--namespace` 要扫描的 namespace；空值表示所有 namespace
- `--output` 输出格式：`table` 或 `json`
- `--include-pods` 在 JSON 输出中包含逐 Pod 明细
- `--max-samples` table 输出中每个应用最多展示的 Pod 样例数量
- `--skip-secret-inspection` 跳过 Secret 列表和内容检查

## 输出

Table 输出会列出引用了含 kubeconfig 的 ConfigMap 或 Secret 的应用：

```text
NAMESPACE  OWNER           CONFIG_RESOURCES                         PODS  SAMPLE_PODS
prod       Deployment/api  ConfigMap/kubeconfigs[admin.conf:plain]  2     api-0,api-1
```

JSON 输出还会包含 `unreferencedResources`，用于展示扫描到含 kubeconfig 但未被任何 Pod 引用的 ConfigMap 或 Secret。

## RBAC

应用归属统计所需的最小只读权限：

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: check-kubeconfig-in-config
rules:
  - apiGroups: [""]
    resources: ["pods", "configmaps"]
    verbs: ["list"]
  - apiGroups: ["apps"]
    resources: ["replicasets"]
    verbs: ["list"]
  - apiGroups: ["batch"]
    resources: ["jobs"]
    verbs: ["list"]
```

如需检查存放在 Secret 中的 kubeconfig 内容，增加以下权限：

```yaml
- apiGroups: [""]
  resources: ["secrets"]
  verbs: ["list"]
```

当 kubeconfig 不应读取 Secret 时使用 `--skip-secret-inspection`。没有 Secret 权限时，扫描器仍会检测 ConfigMap 中的 kubeconfig 并归属到引用它们的应用，但会跳过 Secret 发现。

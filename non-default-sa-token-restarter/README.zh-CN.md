# non-default-sa-token-restarter

查找节点上使用非 `default` ServiceAccount token 的活跃 Kubernetes Pod，并可
删除这些 Pod，让对应控制器重新创建它们。

工具会检测以下 token 使用方式：

- Pod 使用非 `default` ServiceAccount，且最终的
  `automountServiceAccountToken=true`；
- 显式配置的 projected `serviceAccountToken` volume；
- 通过 volume、projected volume 或环境变量引用的 legacy
  `kubernetes.io/service-account-token` Secret。

默认是 dry-run。工具会跳过尚未调度、正在删除以及终态（`Succeeded` 或
`Failed`）的 Pod。工具不要求 Pod 必须由控制器管理；删除没有控制器的 Pod
是不可逆的。

## 构建

```bash
go build -o non-default-sa-token-restarter .
```

## 使用

列出所有节点上的匹配 Pod：

```bash
./non-default-sa-token-restarter
```

只扫描一个节点和 namespace：

```bash
./non-default-sa-token-restarter --node worker-1 --namespace prod
```

确认 dry-run 输出后，删除匹配的 Pod：

```bash
./non-default-sa-token-restarter --node worker-1 --confirm
```

选项：

- `--kubeconfig` kubeconfig 路径；不可用时回退到 in-cluster 配置
- `--context` 要使用的 kubeconfig context
- `--namespace` 要扫描的 namespace；为空表示所有 namespace
- `--node` 要扫描的精确节点名；为空表示所有已调度节点
- `--exclude-pod` 以 `namespace/name` 形式指定要跳过的 Pod；设置
  `POD_NAMESPACE` 和 `POD_NAME` 时，默认跳过这两个变量标识的 Pod
- `--confirm` 删除匹配的 Pod；不传时只输出删除计划
- `--skip-secret-inspection` 不列出 Secret；可能遗漏 legacy token Secret 引用

## Kubernetes Job

`deploy/` 目录包含一次性 Job、专用 ServiceAccount，以及列出 Pod、
ServiceAccount、Secret 和删除 Pod 所需的最小 ClusterRole 权限。Job 默认是
dry-run：

```bash
kubectl apply -k deploy
kubectl logs -n non-default-sa-token-restarter job/non-default-sa-token-restarter
```

Job 使用 Downward API 设置 `POD_NAMESPACE` 和 `POD_NAME`，所以即使之后给容器
参数增加 `--confirm`，也不会选择 Job 自己的 Pod。

确认 dry-run 输出后，如需执行删除，只在 `deploy/job.yaml` 的容器参数中加入
`--confirm`。需要时可以用 Kustomize 覆盖镜像。

## 安全与权限

- 未显式传入 `--confirm` 时不会删除任何 Pod。
- `--node` 精确匹配 `spec.nodeName`；默认扫描所有已调度节点。
- 如果无法检查 Secret，程序会告警并继续执行 Pod 和 ServiceAccount 的直接
  检测；如果集群有意禁止读取 Secret，可使用 `--skip-secret-inspection`。
- 运行身份需要对 Pod、ServiceAccount、Secret 具有 `list` 权限，并对 Pod
  具有 `delete` 权限。使用 `--skip-secret-inspection` 时不需要 Secret 的
  `list` 权限。

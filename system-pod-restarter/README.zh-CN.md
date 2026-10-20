# system-pod-restarter

扫描所有名称不是 `ns-` 开头的 namespace，找出由 Deployment 和 DaemonSet
管理的 Pod，并可删除这些 Pod，让对应控制器重新创建它们。

工具根据 Kubernetes owner reference 判断归属。Deployment 的 Pod 会通过其
ReplicaSet owner 查找；StatefulSet、Job 管理的 Pod 以及没有控制器的裸 Pod
不会被选中。

## 构建

```bash
go build -o system-pod-restarter .
```

## 使用

默认是 dry-run，只输出待删除清单：

```bash
./system-pod-restarter
```

删除匹配的 Pod：

```bash
./system-pod-restarter --confirm
```

需要时可以指定其他 namespace 排除前缀：

```bash
./system-pod-restarter --exclude-namespace-prefix platform- --confirm
```

选项：

- `--kubeconfig` kubeconfig 路径；不可用时回退到 in-cluster 配置
- `--context` 要使用的 kubeconfig context
- `--exclude-namespace-prefix` 跳过名称以此前缀开头的 namespace，默认是 `ns-`
- `--confirm` 删除选中的 Pod；不传时只输出删除计划

## Kubernetes Job

`deploy/` 目录包含一个一次性 Job 以及所需的最小 ClusterRole 权限：列出
namespace、Deployment、DaemonSet、ReplicaSet 和 Pod，并删除 Pod。Job 默认是
dry-run。确认 dry-run 输出后，才在 `deploy/job.yaml` 的容器参数中加入
`--confirm`。

```bash
kubectl apply -k deploy
kubectl logs -n system-pod-restarter job/system-pod-restarter
```

## 安全与权限

- 默认排除名称以 `ns-` 开头的 namespace。
- 只选择由 Deployment 或 DaemonSet 管理的 Pod。
- 默认模式不会删除任何资源，必须显式传入 `--confirm`。
- 删除 Pod 不会删除其 Deployment、DaemonSet、ReplicaSet 或 namespace。
- 运行身份需要对 namespace、Pod、Deployment、DaemonSet、ReplicaSet 具有
  `list` 权限，并对 Pod 具有 `delete` 权限。

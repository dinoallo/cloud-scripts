# orphaned-pvc-scanner

扫描 Kubernetes 中没有 ownerReferences、且当前没有被活跃 Pod 使用的 PVC。这个
工具是只读的，不会删除 PVC 或 PV。

它比 `template-pvc-scanner` 扫描范围更宽：不需要 template instance、
StatefulSet 名称或 claim template。默认情况下，它只使用 PVC metadata、绑定 PV
metadata 和 Pod volume 引用，不会解析 ownerReferences，也不会查询任意 owner
资源类型；只有显式设置 `--resolve-owners` 时才会做深度 owner 查询。

## 构建

```bash
go build -o orphaned-pvc-scanner .
```

## 使用

默认扫描所有 namespace：

```bash
./orphaned-pvc-scanner --output csv > orphaned-pvcs.csv
```

限制到单个 namespace：

```bash
./orphaned-pvc-scanner --namespace prod --output table
```

后续自动化处理可以使用 JSON Lines：

```bash
./orphaned-pvc-scanner --output jsonl > orphaned-pvcs.jsonl
```

当 scanner 对相关 owner 资源类型有读取权限时，可以显式打开深度 ownerReference
解析：

```bash
./orphaned-pvc-scanner --resolve-owners --output csv > orphaned-pvcs.csv
```

## Owner 状态

scanner 每个 PVC 候选对象输出一行。默认情况下，`ownerStatus` 只会是：

- `noOwnerReferences`：PVC 没有 ownerReferences，且没有被任何非终态 Pod 引用。

设置 `--resolve-owners` 后，才可能额外输出这些状态：

- `ownerNotFound`：引用的 owner 对象不存在。
- `ownerUIDMismatch`：同名 owner 对象存在，但 UID 和 ownerReference 里的 UID
  不一致。
- `ownerGVKNotFound`：ownerReference 的 apiVersion/kind 在 Kubernetes
  discovery 中不可用。
- `ownerInvalidScope`：引用的 namespaced owner UID 出现在另一个 namespace。
  PVC 只能引用同 namespace 的 namespaced owner，或 cluster-scoped owner。
- `ownerLookupError`：scanner 无法确认 owner，通常是 RBAC 或 API 查询失败。
  这表示未知状态，不应作为删除信号。

如果任意一个 ownerReference 能解析到 UID 匹配的现存 owner 对象，这个 PVC 就不会
被报告。未设置 `--resolve-owners` 时，带 ownerReferences 的 PVC 会被直接跳过，
不会查询它的 owner 对象。

对于没有 ownerReferences 的 PVC，scanner 会在扫描范围内列出 Pod，并跳过被
非 `Succeeded` / `Failed` 状态 Pod 引用的 PVC。默认模式和 `--resolve-owners`
模式都会应用这个过滤。

## 输出

CSV 和 table 输出使用这些便于人工审阅的字段：

```text
namespace,pvc,pv,ownerStatus,reason,pvcPhase,pvPhase,pvReclaimPolicy,pvClaimRefMatched,pvcStorageClass
```

当 PVC 有 `spec.volumeName` 且对应 PV 出现在集群 PV 列表中时，会填充 PV 字段。
scanner 会一次列出 PV 后在本地匹配，而不是对每个候选 PVC 单独请求一次 PV。
`pvClaimRefMatched` 表示 PV 是否仍然反向指向这个 PVC。

JSON Lines 输出仍保留完整 row object，包括 ownerReference 详情、PVC size 和
PVC age，方便后续自动化处理。

## 安全说明

`noOwnerReferences` 是低置信度信号。很多正常 PVC 本来就不会设置
ownerReferences，例如手工创建的 PVC，或者由某些部署工具管理但没有设置
ownerReferences 的 PVC。
为了减少噪音，scanner 不会报告仍被活跃 Pod 引用的无 ownerReferences PVC。

`ownerNotFound`、`ownerUIDMismatch`、`ownerGVKNotFound` 和
`ownerInvalidScope` 是更强的 orphan 信号，但只有设置 `--resolve-owners` 后才会
出现，删除前仍然应该结合业务和 workload 上下文人工复核。

## Kubernetes Job

以一次性 Job 在集群内运行：

```bash
kubectl apply -k deploy
kubectl logs -n orphaned-pvc-scanner job/orphaned-pvc-scanner
```

Job 会把 CSV 输出到 stdout。需要本地审阅文件时，可以重定向 logs：

```bash
kubectl logs -n orphaned-pvc-scanner job/orphaned-pvc-scanner > orphaned-pvcs.csv
```

默认 RBAC 是只读且刻意收窄的权限：可以列 PVC、列 PV，并列 Pod 以避免报告
仍在使用中的无 ownerReferences PVC。默认 RBAC 不授予 discovery 或 owner 资源
权限。如果要启用 `--resolve-owners`，需要给这个 ServiceAccount 额外授予对应
discovery 和 owner 资源类型的只读 `get`/`list` 权限。

## 参数

- `--kubeconfig` kubeconfig 路径；可用时回退到 in-cluster config
- `--namespace` 要扫描的 namespace；为空表示扫描所有 namespace
- `--output` 输出格式：`table`、`csv` 或 `jsonl`；默认是 `table`
- `--resolve-owners` 通过查询 owner 对象解析 ownerReferences；默认关闭

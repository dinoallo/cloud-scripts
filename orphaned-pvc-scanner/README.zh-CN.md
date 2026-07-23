# orphaned-pvc-scanner

扫描 Kubernetes 中 owner 关系缺失、断裂或无法确认的 PVC。这个工具是只读的，不会
删除 PVC 或 PV。

它比 `template-pvc-scanner` 扫描范围更宽：不需要 template instance、
StatefulSet 名称或 claim template。只要 PVC 没有 ownerReferences，或者
ownerReferences 不能解析到 UID 匹配的现存 owner 对象，就会被报告。

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

## Owner 状态

scanner 每个 PVC 候选对象输出一行。`ownerStatus` 包括：

- `noOwnerReferences`：PVC 没有 ownerReferences。
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
被报告。

## 输出

结构化输出使用固定字段：

```text
namespace,pvc,pv,ownerStatus,ownerAPIVersion,ownerKind,ownerNamespace,ownerName,ownerUID,ownerController,ownerBlockOwnerDeletion,ownerRefCount,reason,pvcPhase,pvPhase,pvReclaimPolicy,pvClaimRefMatched,pvcStorageClass,pvcSize,pvcAge
```

当 PVC 有 `spec.volumeName` 且 scanner 能读取对应 PV 时，会填充 PV 字段。
`pvClaimRefMatched` 表示 PV 是否仍然反向指向这个 PVC。

## 安全说明

`noOwnerReferences` 是低置信度信号。很多正常 PVC 本来就不会设置
ownerReferences，例如手工创建的 PVC，或者由某些部署工具管理但没有设置
ownerReferences 的 PVC。

`ownerNotFound`、`ownerUIDMismatch`、`ownerGVKNotFound` 和
`ownerInvalidScope` 是更强的 orphan 信号，但删除前仍然应该结合业务和 workload
上下文人工复核。

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

默认 RBAC 是只读权限，可以验证常见 workload owner，例如 Pod、
ReplicationController、Deployment、ReplicaSet、StatefulSet、DaemonSet、Job 和
CronJob。如果 PVC 的 owner 是 CRD 或其他资源类型，需要给这个 ServiceAccount
额外授予对应 owner 资源的只读 `get`/`list` 权限；否则这些 PVC 可能会被报告为
`ownerLookupError`。

## 参数

- `--kubeconfig` kubeconfig 路径；可用时回退到 in-cluster config
- `--namespace` 要扫描的 namespace；为空表示扫描所有 namespace
- `--output` 输出格式：`table`、`csv` 或 `jsonl`；默认是 `table`

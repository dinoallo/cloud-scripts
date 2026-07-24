# pvc-cleaner

根据已经人工审核过的 `namespace,pvc` 清单删除 PVC。

`pvc-cleaner` 不扫描集群，也不判断某个 PVC 是否残留。先用
`template-pvc-scanner` 扫描，人工审阅并收敛 CSV 后，再把清单交给 cleaner。

## 构建

```bash
go build -o pvc-cleaner .
```

## 输入

输入可以是最小 CSV：

```csv
namespace,pvc
prod,data-mysql-0
prod,data-redis-0
```

也可以直接使用 `template-pvc-scanner` 产出的 CSV；`pvc-cleaner` 只读取
`namespace` 和 `pvc` 两列，其余字段会被忽略：

```csv
namespace,pvc,path,value,reason,pvcPhase,pvcStorageClass,pvcSize,pvcAge
prod,data-mysql-0,/data,10,reviewed,Bound,fast,10Gi,2d1h
```

无表头输入也支持每行 `namespace,pvc` 或 `namespace/pvc`。重复 PVC 目标只会处理
第一次出现的记录。

## 用法

默认是 dry-run。工具会读取目标清单，并检查每个 PVC 当前是否存在：

```bash
./pvc-cleaner --input reviewed-pvcs.csv
```

删除已经审核过的 PVC：

```bash
./pvc-cleaner --input reviewed-pvcs.csv --confirm
```

从 stdin 读取：

```bash
cat reviewed-pvcs.csv | ./pvc-cleaner --input -
```

## Kubernetes Job

`deploy/` 目录包含一个 Job，会把人工审核后的 CSV 通过 ConfigMap 挂载到
`/input/targets.csv`。应用 Job 前，先用审核后的目标清单替换
`deploy/targets.example.csv`。

应用 dry-run Job：

```bash
kubectl apply -k deploy
kubectl -n pvc-cleaner logs job/pvc-cleaner -c cleaner
```

如果要真正删除，编辑 `deploy/job.yaml`，在 container args 的
`--input /input/targets.csv` 后面追加 `--confirm`，然后重新应用 Job。

## 权限

Job RBAC 只授予 PVC 的以下权限：

```text
resources: persistentvolumeclaims
verbs: get, delete
```

它没有 PVC 或 Pod 的 `list` 权限，也没有任何 PV 权限。PVC 删除后，绑定 PV 是删除
还是保留，由 PV reclaim policy 决定。

如果要进一步收敛生产权限，可以按 namespace 拆分审核后的 CSV，并改用
namespace-scoped Role，而不是默认的 ClusterRole。

## 安全规则

- 工具永远不会自行发现删除目标。
- 必须传入 `--confirm` 才会删除。
- 不存在的 PVC 会输出为 `status=not-found` 并跳过。
- 工具不会 list、get 或 delete PV。

## 参数

- `--kubeconfig` kubeconfig 路径；默认优先使用 in-cluster config
- `--input` 目标 CSV 路径；使用 `-` 表示从 stdin 读取
- `--confirm` 删除清单中的 PVC；不传时只输出 dry-run 计划
- `--wait` 每个 PVC 删除最多等待多久

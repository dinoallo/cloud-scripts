# template-pvc-cleaner

清理 Sealos 模板商店 / applaunchpad PVC 删除 bug 遗留的 PVC/PV 对象。

受影响的本地存储 PVC 来自 StatefulSet `volumeClaimTemplates`。这些模板带有
`path` 和 `value` annotations，但缺少删除逻辑依赖的归属 labels。应用或模板
实例删除后，StatefulSet 可能已经被删掉，但它创建的 PVC 和绑定 PV 仍然残留。

cleaner 不查询 StatefulSet，也不需要 StatefulSet 名称或 template instance 名称。
该工具默认只做 dry-run，只有传入 `--confirm` 才会真正删除资源。

## 构建

```bash
go build -o template-pvc-cleaner .
```

## 用法

先审查单个 namespace。不传 `--confirm` 时，cleaner 只输出计划处理的 PVC/PV
目标：

```bash
./template-pvc-cleaner --namespace prod
```

确认 dry-run 输出后，添加 `--confirm` 删除匹配的 PVC，以及带保护条件的残留 PV：

```bash
./template-pvc-cleaner \
  --namespace prod \
  --confirm
```

如果只想删除 PVC、保留 PV，使用 `--delete-pv=false`：

```bash
./template-pvc-cleaner \
  --namespace prod \
  --delete-pv=false \
  --confirm
```

## 安全规则

cleaner 始终要求 `--namespace`；它不会在一个命令里跨所有 namespace 运行。

只有同时满足以下条件的 PVC 才会被删除：

- PVC 有非空 `path` 和 `value` annotations
- PVC 没有这些归属 labels：`cloud.sealos.io/deploy-on-sealos`、
  `cloud.sealos.io/app-deploy-manager`、`app`
- 同 namespace 下没有活跃 Pod 引用该 PVC

`Succeeded` 或 `Failed` phase 的 Pod 会被视为已结束，不会阻止候选删除。
Pending、Running、Unknown 以及还没有 phase 的 Pod 都会阻止删除。

PV 删除也有保护。绑定 PV 只有在其 `claimRef` 仍指向捕获到的目标 PVC namespace、
name 和 UID 时才会被删除。如果 `claimRef` 发生变化，cleaner 会拒绝删除该 PV。

废弃的范围参数 `--instance`、`--statefulset`、`--claim-template`、
`--discover-orphans` 和 `--allow-name-only` 会被拒绝，而不是静默忽略，因为它们
已经不能收窄 PVC-only 清理范围。

## 参数

- `--kubeconfig` kubeconfig 路径；默认优先使用 in-cluster config
- `--namespace` 包含残留 template PVC 的 namespace
- `--confirm` 删除匹配的 PVC/PV；不传时只输出 dry-run 计划
- `--delete-pv=false` 只删除 PVC，保留 PV
- `--wait` 每个 PVC/PV 删除最多等待多久

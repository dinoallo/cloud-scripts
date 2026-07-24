# template-pvc-scanner

扫描 Sealos 模板商店 / applaunchpad PVC 删除 bug 遗留的 PVC 候选对象。
人工审核 scanner 输出后，再把选中的目标交给 `pvc-cleaner`。

受影响的本地存储 PVC 来自 StatefulSet `volumeClaimTemplates`。这些模板带有
`path` 和 `value` annotations，但缺少删除逻辑依赖的归属 labels。应用或模板
实例删除后，StatefulSet 可能已经被删掉，但它创建的 PVC 和绑定 PV 仍然残留。

该 scanner 不查询 StatefulSet，也不需要 StatefulSet 名称或 template instance
名称。它只列出 PVC 候选对象，不会 list 或 get PV。PVC 删除后 PV 是否删除或保留，
交给 PV reclaim policy 决定。

## 构建

```bash
go build -o template-pvc-scanner .
```

## Kubernetes Job

`deploy/` 目录包含一套只读 Job 配置，会把 CSV 输出写到 pod 内的
`/scan-output/pvc-scan.csv`。

应用前可按需修改 `deploy/job.yaml`：

- 设置 `SCANNER_ARGS` 添加 `--namespace prod` 等参数；留空时扫描所有 namespace
- 如果镜像发布在其他 registry 或 owner 下，替换
  `ghcr.io/dinoallo/template-pvc-scanner:latest`

默认镜像由 `template-pvc-scanner/Dockerfile` 构建，包含
`/template-pvc-scanner` 二进制、wrapper 命令需要的 `/bin/sh`，以及
`kubectl cp` 需要的 `tar`。

应用 Job：

```bash
kubectl apply -k deploy
```

等待日志显示 CSV 已写入：

```bash
kubectl -n template-pvc-scanner logs job/template-pvc-scanner -c scanner
```

为了让 `kubectl cp` 能 exec 到容器里，扫描完成后 Job 的 pod 默认会继续保持
运行。复制 CSV 后删除 Job：

```bash
pod="$(kubectl -n template-pvc-scanner get pod -l job-name=template-pvc-scanner -o jsonpath='{.items[0].metadata.name}')"
kubectl -n template-pvc-scanner cp "$pod:/scan-output/pvc-scan.csv" ./pvc-scan.csv -c scanner
kubectl -n template-pvc-scanner delete job template-pvc-scanner
```

`deploy/rbac.yaml` 是只读权限，只允许 list PVC 和 Pod。

## 用法

默认扫描所有 namespace：

```bash
./template-pvc-scanner --output csv > pvc-scan.csv
```

添加 `--namespace prod` 可以限制到单个 namespace：

```bash
./template-pvc-scanner \
  --namespace prod \
  --output table
```

如果后续要自动处理，JSON Lines 会每行输出一个候选对象：

```bash
./template-pvc-scanner --output jsonl > pvc-scan.jsonl
```

结构化输出字段如下：

```text
namespace,pvc,path,value,reason,pvcPhase,pvcStorageClass,pvcSize,pvcAge
```

## 安全规则

scanner 是 PVC-only 模式。它不会 list 或匹配 StatefulSet，也不会 list 或 get PV。

只有同时满足以下条件的 PVC 才会被输出：

- PVC 有非空 `path` 和 `value` annotations
- PVC 没有这些归属 labels：`cloud.sealos.io/deploy-on-sealos`、
  `cloud.sealos.io/app-deploy-manager`
- 同 namespace 下没有活跃 Pod 引用该 PVC

`Succeeded` 或 `Failed` phase 的 Pod 会被视为已结束，不会阻止候选输出。
Pending、Running、Unknown 以及还没有 phase 的 Pod 都会阻止该 PVC 输出。

scanner 不判断 PV 是否应该清理。PVC 被后续清理步骤删除后，绑定 PV 是删除还是
保留，由该 PV 的 reclaim policy 决定。

## 参数

- `--kubeconfig` kubeconfig 路径；默认优先使用 in-cluster config
- `--namespace` 要扫描的 namespace；为空表示扫描所有 namespace
- `--output` 输出格式：`table`、`csv` 或 `jsonl`；默认 `table`

废弃的范围参数 `--instance`、`--statefulset`、`--claim-template` 和
`--discover-orphans` 为了兼容旧 wrapper 仍会被解析，但已经不参与筛选。

# template-pvc-scanner

扫描 Sealos template frontend PVC 删除 bug 可能遗留的 PVC/PV 候选对象。它是
`template-pvc-cleaner` 的只读配套工具。

受影响的 template frontend 版本可能会创建带有
`cloud.sealos.io/deploy-on-sealos=<instance>` 标签的 StatefulSet，但
StatefulSet 的 `volumeClaimTemplates` 没有把这个标签复制到生成的 PVC 上。
实例删除逻辑按 instance label 删除 PVC 时就会漏掉这些 StatefulSet PVC。
StatefulSet 可能已经被垃圾回收删除，但 PVC 以及它绑定的 PV 会残留。

该 scanner 没有删除路径，也没有 `--confirm` 参数。它只列出候选对象，并检查绑定
PV 是否仍然指向候选 PVC。

## 构建

```bash
go build -o template-pvc-scanner .
```

## Kubernetes Job

`deploy/` 目录包含一套只读 Job 配置，会把 CSV 输出写到 pod 内的
`/scan-output/pvc-scan.csv`。

apply 之前先编辑 `deploy/job.yaml`：

- 把 `registry.example.com/template-pvc-scanner:latest` 替换成你的 scanner
  镜像
- 设置 `TEMPLATE_INSTANCE`
- 如需限制 namespace 或显式指定 StatefulSet / claim template，调整
  `SCANNER_ARGS`

镜像需要把 scanner 二进制放在 `/template-pvc-scanner`，或者通过 `SCANNER_BIN`
指定路径。镜像里还需要 `/bin/sh` 执行 wrapper 命令，并且需要 `tar` 支持
`kubectl cp`。

应用 Job：

```bash
kubectl apply -k deploy
```

等日志里出现 CSV 已写入的提示：

```bash
kubectl -n template-pvc-scanner logs job/template-pvc-scanner -c scanner
```

为了让 `kubectl cp` 能 exec 到容器里，扫描完成后 Job 的 pod 默认会继续保持
Running。复制完 CSV 后再删除 Job：

```bash
pod="$(kubectl -n template-pvc-scanner get pod -l job-name=template-pvc-scanner -o jsonpath='{.items[0].metadata.name}')"
kubectl -n template-pvc-scanner cp "$pod:/scan-output/pvc-scan.csv" ./pvc-scan.csv -c scanner
kubectl -n template-pvc-scanner delete job template-pvc-scanner
```

`deploy/rbac.yaml` 中的 RBAC 是只读权限，只允许访问 PVC、PV 和 StatefulSet。

## 使用

默认会扫描所有 namespace。加上 `--namespace prod` 时，才会把扫描范围限制到单个
namespace。

当 StatefulSet 仍然存在时，扫描带 template instance label 的 live StatefulSet：

```bash
./template-pvc-scanner \
  --instance my-template-instance
```

当 StatefulSet 已经被删除，但已知 StatefulSet 名称和 volumeClaimTemplate
名称时，显式传入它们：

```bash
./template-pvc-scanner \
  --instance my-template-instance \
  --statefulset mysql \
  --claim-template data
```

当 StatefulSet 已经被删除且名称未知时，使用保守 orphan 发现模式：

```bash
./template-pvc-scanner \
  --instance my-template-instance \
  --discover-orphans
```

输出会包含 PVC、推断出的 StatefulSet、claim template、命中原因、绑定 PV、PV
phase、reclaim policy，以及 PV 的 `claimRef` 是否匹配 PVC。

默认输出是适合直接阅读 Job logs 的表格：

```bash
./template-pvc-scanner \
  --instance my-template-instance \
  --output table
```

生产环境人工审阅时，CSV 更方便放进表格里排序、过滤和标注：

```bash
./template-pvc-scanner \
  --instance my-template-instance \
  --discover-orphans \
  --output csv > pvc-scan.csv
```

后续自动化处理时，JSON Lines 会每行输出一个候选对象：

```bash
./template-pvc-scanner \
  --instance my-template-instance \
  --discover-orphans \
  --output jsonl > pvc-scan.jsonl
```

结构化输出使用固定字段：

```text
namespace,pvc,pv,statefulset,claimTemplate,reason,pvcPhase,pvPhase,pvReclaimPolicy,pvClaimRefMatched,pvcStorageClass,pvcSize,pvcAge
```

## 安全规则

每次扫描都会限定在单个 template instance 中。namespace 是可选参数；不传时会扫
所有 namespace，并且 StatefulSet 和 PVC 的匹配会按 namespace 隔离。

只有 PVC 完全没有 `cloud.sealos.io/deploy-on-sealos` 标签时，才会被视为受
bug 影响。已经带有该标签的 PVC 不会被报告。

live StatefulSet 模式要求：

- StatefulSet 能被 `cloud.sealos.io/deploy-on-sealos=<instance>` 选中
- `volumeClaimTemplate` 缺少同一个标签
- PVC 名字符合 `<claim-template>-<statefulset>-<ordinal>`

显式 orphan 模式要求：

- 提供 `--statefulset` 和 `--claim-template`
- PVC 名字符合 `<claim-template>-<statefulset>-<ordinal>`
- 当 StatefulSet 已经不存在时，还需要 legacy `app=<statefulset>` 或
  `app=<instance>` 证据

`--discover-orphans` 模式要求：

- PVC 没有 `cloud.sealos.io/deploy-on-sealos` 标签
- PVC 有 legacy `app=<statefulset>` 标签
- PVC 名字符合 `<claim-template>-<statefulset>-<ordinal>`
- 同一个 namespace 中当前不存在同名 `app` 对应的 live StatefulSet

## 参数

- `--kubeconfig` kubeconfig 路径；可用时回退到 in-cluster config
- `--namespace` 要扫描的 namespace；为空表示扫描所有 namespace
- `--instance` template instance 名称
- `--output` 输出格式：`table`、`csv` 或 `jsonl`；默认是 `table`
- `--statefulset` 要检查的 StatefulSet 名称；可重复
- `--claim-template` volumeClaimTemplate 名称；可重复
- `--discover-orphans` 通过 legacy `app` label 发现 orphan StatefulSet PVC

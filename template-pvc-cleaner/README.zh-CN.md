# template-pvc-cleaner

清理 Sealos template frontend PVC 删除 bug 遗留的 PVC/PV。

受影响的 template frontend 版本可能会创建带有
`cloud.sealos.io/deploy-on-sealos=<instance>` 标签的 StatefulSet，但
StatefulSet 的 `volumeClaimTemplates` 没有把这个标签复制到生成的 PVC 上。
实例删除逻辑按 instance label 删除 PVC 时就会漏掉这些 StatefulSet PVC。
StatefulSet 可能已经被垃圾回收删除，但 PVC 以及它绑定的 PV 会残留。

该工具默认只做 dry-run。只有传入 `--confirm` 才会真正删除资源。

## 构建

```bash
go build -o template-pvc-cleaner .
```

## 使用

当 StatefulSet 仍然存在时，工具可以通过带 template instance label 的 live
StatefulSet 自动发现受影响 PVC：

```bash
./template-pvc-cleaner \
  --namespace prod \
  --instance my-template-instance
```

当 StatefulSet 已经被删除，但已知 StatefulSet 名称和 volumeClaimTemplate
名称时，显式传入它们：

```bash
./template-pvc-cleaner \
  --namespace prod \
  --instance my-template-instance \
  --statefulset mysql \
  --claim-template data
```

当 StatefulSet 已经被删除且名称未知时，使用保守 orphan 发现模式：

```bash
./template-pvc-cleaner \
  --namespace prod \
  --instance my-template-instance \
  --discover-orphans
```

先审查 dry-run 输出。确认目标列表无误后再添加 `--confirm`：

```bash
./template-pvc-cleaner \
  --namespace prod \
  --instance my-template-instance \
  --statefulset mysql \
  --claim-template data \
  --confirm
```

## 安全规则

每次运行都限定在单个 namespace 和单个 template instance 中。

只有 PVC 完全没有 `cloud.sealos.io/deploy-on-sealos` 标签时，才会被视为受
bug 影响。已经带有该标签的 PVC 不会被删除。

live StatefulSet 模式要求：

- StatefulSet 能被 `cloud.sealos.io/deploy-on-sealos=<instance>` 选中
- `volumeClaimTemplate` 缺少同一个标签
- PVC 名字符合 `<claim-template>-<statefulset>-<ordinal>`

显式 orphan 模式要求：

- 提供 `--statefulset` 和 `--claim-template`
- PVC 名字符合 `<claim-template>-<statefulset>-<ordinal>`
- 默认还需要 legacy `app=<statefulset>` 或 `app=<instance>` 证据

`--discover-orphans` 模式要求：

- PVC 没有 `cloud.sealos.io/deploy-on-sealos` 标签
- PVC 有 legacy `app=<statefulset>` 标签
- PVC 名字符合 `<claim-template>-<statefulset>-<ordinal>`
- 当前不存在同名 `app` 对应的 live StatefulSet

PV 删除也有保护。只有绑定 PV 的 `claimRef` 仍然指向捕获到的目标 PVC 的
namespace、name 和 UID 时，才会删除该 PV。

生产环境不建议使用 `--allow-name-only`，除非已经人工确认目标 PVC。该参数会放宽显式
orphan 清理中的 legacy label 证据要求。

## 参数

- `--kubeconfig` kubeconfig 路径；可用时回退到 in-cluster config
- `--namespace` 包含残留 template PVC 的 namespace
- `--instance` template instance 名称
- `--statefulset` 要检查的 StatefulSet 名称；可重复
- `--claim-template` volumeClaimTemplate 名称；可重复
- `--discover-orphans` 通过 legacy `app` label 发现 orphan StatefulSet PVC
- `--confirm` 删除匹配的 PVC/PV；不传时只输出 dry-run 计划
- `--delete-pv=false` 只删除 PVC，保留 PV
- `--allow-name-only` 允许显式 orphan 清理不要求 legacy label 证据
- `--wait` 每个 PVC/PV 删除的最长等待时间

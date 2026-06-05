# check-legacy-sa-token

扫描 Kubernetes Pod，报告仍在引用旧式 Secret 型 ServiceAccount token 的应用。应用归并规则与 `check-kubeconfig-in-config` 一致：优先使用 Pod 的 controller ownerReference；没有 controller 时使用第一个 ownerReference；没有 ownerReference 的独立 Pod 记为 `Pod/<name>`。

扫描器把 `type` 为 `kubernetes.io/service-account-token` 的 Secret 识别为旧式 ServiceAccount token Secret，然后检查 Pod 是否通过以下方式引用这些 Secret：

- Secret volume
- projected volume 里的 Secret source
- `envFrom` Secret 引用
- 单个环境变量的 Secret key 引用

## 构建

```bash
go build -o check-legacy-sa-token .
```

## 使用

```bash
./check-legacy-sa-token
./check-legacy-sa-token --namespace prod
./check-legacy-sa-token --context my-cluster --output json --include-pods
```

默认输出 CSV，列如下：

```text
namespace,ownerKind,ownerName,serviceAccounts,tokenServiceAccounts,tokenSecrets
```

使用 `--output table` 可输出更适合人工阅读的表格。JSON 输出会包含 `unreferencedTokenSecrets`，用于查看扫描到但未被任何 Pod 引用的旧式 ServiceAccount token Secret。

参数：

- `--kubeconfig` kubeconfig 路径；不可用时回退到 in-cluster config
- `--context` 使用的 kubeconfig context
- `--namespace` 要扫描的 namespace；空值表示所有 namespace
- `--output` 输出格式：`csv`、`table` 或 `json`
- `--include-pods` 在 JSON 输出中包含逐 Pod 明细
- `--max-samples` table 输出中每个应用最多展示的 Pod 样例数量

## Kubernetes

以集群内一次性 Job 运行：

```bash
kubectl apply -k deploy
kubectl logs -n check-legacy-sa-token job/check-legacy-sa-token
```

部署目录也包含每天集群时间 02:00 运行的 CronJob：

```bash
kubectl get cronjob -n check-legacy-sa-token check-legacy-sa-token
```

镜像覆盖和 RBAC 说明见 [deploy/README.md](deploy/README.md)。

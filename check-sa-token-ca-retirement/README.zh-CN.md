# check-sa-token-ca-retirement

在根 CA / `sa.key` 轮转后，扫描 Kubernetes 工作负载中阻碍移除旧根 CA 和旧 ServiceAccount 公钥材料的对象。

扫描器对应轮转流程里的退出检查：

- Pod 内 `ca.crt` 应包含指定的新 CA 证书材料。
- projected ServiceAccount token 的 `iat` 应晚于新签名 key 切换时间；或旧 token 已经过期。
- legacy `kubernetes.io/service-account-token` Secret 不应仍被 Pod 引用，因为这类 token 通常不会自然过期。

应用按 Pod 的 ownerReference 聚合。扫描器优先使用 `controller=true` 的 ownerReference；没有 controller owner 时使用第一个 ownerReference；没有 ownerReference 时按 `Pod/<name>` 输出。

## 构建

```bash
go build -o check-sa-token-ca-retirement .
```

## 使用

```bash
./check-sa-token-ca-retirement
./check-sa-token-ca-retirement --namespace prod
./check-sa-token-ca-retirement --output json --include-pods
./check-sa-token-ca-retirement --sa-key-cutover 2026-06-08T08:00:00Z
./check-sa-token-ca-retirement --new-ca-file ./new-ca.pem --sa-key-cutover 2026-06-08T08:00:00Z
```

默认会列出 Pods、ServiceAccounts 和 legacy ServiceAccount token Secrets。只有传入 `--new-ca-file` 或 `--sa-key-cutover` 时才会读取 Pod 内文件，因为这些检查需要 `pods/exec`。

选项：

- `--kubeconfig` kubeconfig 路径；不可用时回退到 in-cluster config
- `--context` 要使用的 kubeconfig context
- `--namespace` 要扫描的 namespace；空值表示所有 namespace
- `--output` 输出格式：`csv`、`table` 或 `json`
- `--include-pods` 在 JSON 输出中包含逐 Pod 明细
- `--max-samples` table 输出中每个应用最多展示的 Pod 样例数量
- `--new-ca-file` 包含新 CA 证书材料的 PEM 文件
- `--sa-key-cutover` 新 ServiceAccount 签名 key 开始签发 token 的 RFC3339 时间
- `--ca-cert-path` 容器内 `ca.crt` 路径，默认 `/var/run/secrets/kubernetes.io/serviceaccount/ca.crt`
- `--token-path` 容器内默认 projected token 路径，默认 `/var/run/secrets/kubernetes.io/serviceaccount/token`
- `--skip-pod-exec` 即使设置了 CA 或 cutover 参数，也跳过 Pod 文件检查
- `--skip-legacy-secret-inspection` 跳过 Secret 列表检查

## 输出

默认输出为 CSV，包含以下列：

```text
namespace,ownerKind,ownerName,serviceAccounts,issueTypes,maxBlockingTokenExpiration,legacyTokenSecrets,pods
```

常见 issue 类型：

- `ca_crt_missing_required_ca`
- `ca_crt_read_failed`
- `ca_crt_parse_failed`
- `projected_token_issued_before_cutover`
- `projected_token_read_failed`
- `projected_token_decode_failed`
- `projected_token_missing_iat`
- `legacy_secret_token_in_use`
- `pod_not_inspectable`

使用 `--output table` 可以输出更宽的可读表格。JSON 输出还会包含 `unreferencedLegacyTokenSecrets`，用于展示扫描到但未被任何 Pod 引用的 legacy ServiceAccount token Secret。

## Kubernetes

在集群内以一次性 Job 运行扫描器：

```bash
kubectl apply -k deploy
kubectl logs -n check-sa-token-ca-retirement job/check-sa-token-ca-retirement
```

同一个部署目录也包含每天集群时间 02:00 运行的 CronJob：

```bash
kubectl get cronjob -n check-sa-token-ca-retirement check-sa-token-ca-retirement
```

镜像覆盖、CA bundle 挂载和 RBAC 说明见 [deploy/README.zh-CN.md](deploy/README.zh-CN.md)。

## RBAC

legacy token 检查所需的最小只读权限：

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: check-sa-token-ca-retirement
rules:
  - apiGroups: [""]
    resources: ["pods", "serviceaccounts", "secrets"]
    verbs: ["list"]
```

使用 `--new-ca-file` 或 `--sa-key-cutover` 时还需要 `pods/exec`：

```yaml
- apiGroups: [""]
  resources: ["pods/exec"]
  verbs: ["create"]
```

当 kubeconfig 不应读取 Secret 时使用 `--skip-legacy-secret-inspection`。没有 Secret 权限时会跳过 legacy Secret token 发现。

# Kubernetes 部署

这些清单会以一次性 `Job` 或定时 `CronJob` 运行 `check-sa-token-ca-retirement`。

应用默认资源：

```bash
kubectl apply -k deploy
```

清单会创建独立的 `check-sa-token-ca-retirement` namespace。

从 CronJob 模板发起一次临时扫描：

```bash
kubectl create job -n check-sa-token-ca-retirement --from=cronjob/check-sa-token-ca-retirement check-sa-token-ca-retirement-manual
kubectl logs -n check-sa-token-ca-retirement job/check-sa-token-ca-retirement-manual
```

Job 和 CronJob 日志默认使用 CSV 输出：

```text
namespace,ownerKind,ownerName,serviceAccounts,issueTypes,maxBlockingTokenExpiration,legacyTokenSecrets,pods
```

用同名 Job 重新运行前，先删除一次性 Job：

```bash
kubectl delete job -n check-sa-token-ca-retirement check-sa-token-ca-retirement
kubectl apply -k deploy
```

默认镜像：

```text
ghcr.io/dinoallo/check-sa-token-ca-retirement:latest
```

用 Kustomize 覆盖镜像：

```bash
kustomize edit set image ghcr.io/dinoallo/check-sa-token-ca-retirement=ghcr.io/YOUR_ORG/check-sa-token-ca-retirement:TAG
```

## 启用 CA 和 projected token 检查

默认清单会报告 legacy ServiceAccount token Secret，以及仍引用它们的 Pod。如需同时检查 Pod 内 `ca.crt` 和当前 projected token，在 `job.yaml` 或 `cronjob.yaml` 增加参数，例如：

```yaml
args:
  - --new-ca-file=/etc/check-sa-token-ca-retirement/new-ca.pem
  - --sa-key-cutover=2026-06-08T08:00:00Z
  - --output=json
```

请用 ConfigMap 或 Secret 等方式把 `new-ca.pem` 挂载到扫描器 Pod。该文件必须是 PEM 格式的新 CA 证书材料。

## RBAC

默认 RBAC 包含：

- 对 `pods`、`serviceaccounts`、`secrets` 的 `list`
- 对 `pods/exec` 的 `create`

只有使用 `--new-ca-file` 或 `--sa-key-cutover` 时才需要 `pods/exec`。如果只检查 legacy token Secret，可以移除 `pods/exec` 规则。如果不允许读取 Secret，移除 `secrets` 并添加 `--skip-legacy-secret-inspection`，此时会跳过 legacy Secret token 发现。

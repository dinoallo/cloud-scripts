# Kubernetes 部署

这些清单会把 `check-legacy-sa-token` 作为一次性 `Job` 或定时 `CronJob` 运行。

应用默认资源：

```bash
kubectl apply -k deploy
```

清单会创建专用的 `check-legacy-sa-token` namespace。

从 CronJob 模板发起一次临时扫描：

```bash
kubectl create job -n check-legacy-sa-token --from=cronjob/check-legacy-sa-token check-legacy-sa-token-manual
kubectl logs -n check-legacy-sa-token job/check-legacy-sa-token-manual
```

Job 和 CronJob 日志默认使用 CSV 输出：

```text
namespace,ownerKind,ownerName,serviceAccounts,tokenServiceAccounts,tokenSecrets
```

用相同名称重新运行一次性 Job 前，先删除旧 Job：

```bash
kubectl delete job -n check-legacy-sa-token check-legacy-sa-token
kubectl apply -k deploy
```

默认镜像：

```text
ghcr.io/dinoallo/check-legacy-sa-token:latest
```

用 Kustomize 覆盖镜像：

```bash
kustomize edit set image ghcr.io/dinoallo/check-legacy-sa-token=ghcr.io/YOUR_ORG/check-legacy-sa-token:TAG
```

RBAC 包含读取 Pods 和 Secrets 的权限。读取 Secrets 是必需的，因为扫描器需要检查 Secret 类型和 ServiceAccount token 相关注解。

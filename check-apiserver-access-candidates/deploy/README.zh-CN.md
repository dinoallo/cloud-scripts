# Kubernetes 部署

这些清单会以一次性 `Job` 或定时 `CronJob` 运行 `check-apiserver-access-candidates`。

应用默认资源：

```bash
kubectl apply -k deploy
```

清单会创建独立的 `check-apiserver-access-candidates` namespace。

从 CronJob 模板发起一次临时扫描：

```bash
kubectl create job -n check-apiserver-access-candidates --from=cronjob/check-apiserver-access-candidates check-apiserver-access-candidates-manual
kubectl logs -n check-apiserver-access-candidates job/check-apiserver-access-candidates-manual
```

Job 和 CronJob 日志默认使用 CSV 输出：

```text
namespace,ownerKind,ownerName,confidence,serviceAccounts,reasons,pods
```

用同名 Job 重新运行前，先删除一次性 Job：

```bash
kubectl delete job -n check-apiserver-access-candidates check-apiserver-access-candidates
kubectl apply -k deploy
```

默认镜像：

```text
ghcr.io/dinoallo/check-apiserver-access-candidates:latest
```

用 Kustomize 覆盖镜像：

```bash
kustomize edit set image ghcr.io/dinoallo/check-apiserver-access-candidates=ghcr.io/YOUR_ORG/check-apiserver-access-candidates:TAG
```

默认 RBAC 包含对 Pods、ServiceAccounts、ConfigMaps、Secrets、RoleBindings、ClusterRoleBindings 的只读权限。如果集群不允许读取 Secret，可以从 `rbac.yaml` 移除 `secrets` 并给 Job 或 CronJob 增加 `--skip-secret-inspection`。没有 Secret 权限时，扫描器仍会检查 ServiceAccount/RBAC/token 信号和 ConfigMap 中的 kubeconfig。

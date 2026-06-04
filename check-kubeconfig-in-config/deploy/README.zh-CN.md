# Kubernetes Deployment

这些 manifest 会以一次性的 `Job` 或定时 `CronJob` 运行 `check-kubeconfig-in-config`。

应用默认资源：

```bash
kubectl apply -k deploy
```

这些 manifest 会创建独立的 `check-kubeconfig-in-config` namespace。

从 CronJob 模板运行一次临时扫描：

```bash
kubectl create job -n check-kubeconfig-in-config --from=cronjob/check-kubeconfig-in-config check-kubeconfig-in-config-manual
kubectl logs -n check-kubeconfig-in-config job/check-kubeconfig-in-config-manual
```

如需用相同名称重新运行一次性 Job，先删除旧 Job：

```bash
kubectl delete job -n check-kubeconfig-in-config check-kubeconfig-in-config
kubectl apply -k deploy
```

默认镜像为：

```text
ghcr.io/dinoallo/check-kubeconfig-in-config:latest
```

使用 Kustomize 覆盖镜像：

```bash
kustomize edit set image ghcr.io/dinoallo/check-kubeconfig-in-config=ghcr.io/YOUR_ORG/check-kubeconfig-in-config:TAG
```

RBAC 默认包含 Secret 只读权限，因此扫描器可以检查存放在 Secret 中的 kubeconfig 内容。如果集群不允许读取 Secret，可以从 `rbac.yaml` 移除 `secrets` 规则，并在 Job 或 CronJob 参数中加入 `--skip-secret-inspection`。没有 Secret 权限时，扫描器仍会检查 ConfigMap 并归属到引用这些 ConfigMap 的应用。

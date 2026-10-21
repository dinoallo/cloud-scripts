# Kubernetes 部署

这些 manifest 会以一次性 Job 运行 `non-default-sa-token-restarter`。

应用默认资源：

```bash
kubectl apply -k deploy
```

Job 默认是 dry-run，可以查看日志：

```bash
kubectl logs -n non-default-sa-token-restarter job/non-default-sa-token-restarter
```

确认输出后，如果需要删除匹配的 Pod，可以在 `job.yaml` 的容器 `args` 中加入
`--confirm`。Job 自己的 Pod 会通过 Downward API 注入信息，并自动排除。

默认镜像为：

```text
ghcr.io/dinoallo/non-default-sa-token-restarter:latest
```

ClusterRole 对 Pod、ServiceAccount 和 Secret 授予 `list` 权限，对 Pod 授予
`delete` 权限。如果集群不允许读取 Secret，请从 `rbac.yaml` 删除 `secrets`，
并在 Job 参数中加入 `--skip-secret-inspection`。

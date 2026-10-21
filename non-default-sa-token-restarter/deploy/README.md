# Kubernetes Deployment

These manifests run `non-default-sa-token-restarter` as a one-shot Job.

Apply the default resources:

```bash
kubectl apply -k deploy
```

The Job is dry-run by default. Review its logs:

```bash
kubectl logs -n non-default-sa-token-restarter job/non-default-sa-token-restarter
```

After reviewing the output, add `--confirm` to the container `args` in
`job.yaml` if the matching Pods should be deleted. The Job's own Pod is passed
through the Downward API and excluded automatically.

The default image is:

```text
ghcr.io/dinoallo/non-default-sa-token-restarter:latest
```

The ClusterRole grants `list` on Pods, ServiceAccounts, and Secrets and `delete`
on Pods. Remove `secrets` from `rbac.yaml` and add `--skip-secret-inspection` to
the Job args if Secret reads are not allowed in the cluster.

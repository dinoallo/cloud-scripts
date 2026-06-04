# Kubernetes Deployment

These manifests run `check-non-default-sa-token` as a one-shot `Job` or as a scheduled `CronJob`.

Apply the default resources:

```bash
kubectl apply -k deploy
```

The manifests create a dedicated `check-non-default-sa-token` namespace.

Run an ad-hoc scan from the CronJob template:

```bash
kubectl create job -n check-non-default-sa-token --from=cronjob/check-non-default-sa-token check-non-default-sa-token-manual
kubectl logs -n check-non-default-sa-token job/check-non-default-sa-token-manual
```

Delete the one-shot Job before rerunning it with the same name:

```bash
kubectl delete job -n check-non-default-sa-token check-non-default-sa-token
kubectl apply -k deploy
```

The default image is:

```text
ghcr.io/dinoallo/check-non-default-sa-token:latest
```

Override it with Kustomize:

```bash
kustomize edit set image ghcr.io/dinoallo/check-non-default-sa-token=ghcr.io/YOUR_ORG/check-non-default-sa-token:TAG
```

The RBAC includes read access to Secrets so the scanner can attribute legacy `kubernetes.io/service-account-token` Secret volumes. Remove the `secrets` resource from `rbac.yaml` and add `--skip-secret-inspection` to the Job or CronJob args if Secret reads are not allowed in your cluster.

Because the scanner runs with its own non-default ServiceAccount, scan results can include the scanner Job-owned Pod itself. Treat that finding as expected unless you choose a different runtime identity.

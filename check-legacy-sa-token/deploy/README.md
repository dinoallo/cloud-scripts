# Kubernetes Deployment

These manifests run `check-legacy-sa-token` as a one-shot `Job` or as a scheduled `CronJob`.

Apply the default resources:

```bash
kubectl apply -k deploy
```

The manifests create a dedicated `check-legacy-sa-token` namespace.

Run an ad-hoc scan from the CronJob template:

```bash
kubectl create job -n check-legacy-sa-token --from=cronjob/check-legacy-sa-token check-legacy-sa-token-manual
kubectl logs -n check-legacy-sa-token job/check-legacy-sa-token-manual
```

Job and CronJob logs use the scanner's default CSV output:

```text
namespace,ownerKind,ownerName,serviceAccounts,tokenServiceAccounts,tokenSecrets
```

Delete the one-shot Job before rerunning it with the same name:

```bash
kubectl delete job -n check-legacy-sa-token check-legacy-sa-token
kubectl apply -k deploy
```

The default image is:

```text
ghcr.io/dinoallo/check-legacy-sa-token:latest
```

Override it with Kustomize:

```bash
kustomize edit set image ghcr.io/dinoallo/check-legacy-sa-token=ghcr.io/YOUR_ORG/check-legacy-sa-token:TAG
```

The RBAC includes read access to Pods and Secrets. Secret access is required because the scanner must inspect Secret types and ServiceAccount token annotations.

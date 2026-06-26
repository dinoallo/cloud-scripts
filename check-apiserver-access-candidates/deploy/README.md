# Kubernetes Deployment

These manifests run `check-apiserver-access-candidates` as a one-shot `Job` or as a scheduled `CronJob`.

Apply the default resources:

```bash
kubectl apply -k deploy
```

The manifests create a dedicated `check-apiserver-access-candidates` namespace.

Run an ad-hoc scan from the CronJob template:

```bash
kubectl create job -n check-apiserver-access-candidates --from=cronjob/check-apiserver-access-candidates check-apiserver-access-candidates-manual
kubectl logs -n check-apiserver-access-candidates job/check-apiserver-access-candidates-manual
```

Job and CronJob logs use the scanner's default CSV output:

```text
namespace,ownerKind,ownerName,confidence,serviceAccounts,reasons,pods
```

Delete the one-shot Job before rerunning it with the same name:

```bash
kubectl delete job -n check-apiserver-access-candidates check-apiserver-access-candidates
kubectl apply -k deploy
```

The default image is:

```text
ghcr.io/dinoallo/check-apiserver-access-candidates:latest
```

Override it with Kustomize:

```bash
kustomize edit set image ghcr.io/dinoallo/check-apiserver-access-candidates=ghcr.io/YOUR_ORG/check-apiserver-access-candidates:TAG
```

The RBAC includes read access to Pods, ServiceAccounts, ConfigMaps, Secrets, RoleBindings, and ClusterRoleBindings. Remove the `secrets` resource from `rbac.yaml` and add `--skip-secret-inspection` to the Job or CronJob args if Secret reads are not allowed in your cluster. Without Secret access, the scanner still checks ServiceAccount/RBAC/token signals and ConfigMap-backed kubeconfigs.

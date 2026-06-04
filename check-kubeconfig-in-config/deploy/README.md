# Kubernetes Deployment

These manifests run `check-kubeconfig-in-config` as a one-shot `Job` or as a scheduled `CronJob`.

Apply the default resources:

```bash
kubectl apply -k deploy
```

The manifests create a dedicated `check-kubeconfig-in-config` namespace.

Run an ad-hoc scan from the CronJob template:

```bash
kubectl create job -n check-kubeconfig-in-config --from=cronjob/check-kubeconfig-in-config check-kubeconfig-in-config-manual
kubectl logs -n check-kubeconfig-in-config job/check-kubeconfig-in-config-manual
```

Job and CronJob logs use the scanner's default CSV output:

```text
namespace,ownerKind,ownerName,serviceAccounts
```

Delete the one-shot Job before rerunning it with the same name:

```bash
kubectl delete job -n check-kubeconfig-in-config check-kubeconfig-in-config
kubectl apply -k deploy
```

The default image is:

```text
ghcr.io/dinoallo/check-kubeconfig-in-config:latest
```

Override it with Kustomize:

```bash
kustomize edit set image ghcr.io/dinoallo/check-kubeconfig-in-config=ghcr.io/YOUR_ORG/check-kubeconfig-in-config:TAG
```

The RBAC includes read access to Secrets so the scanner can inspect kubeconfig content stored in Secrets. Remove the `secrets` resource from `rbac.yaml` and add `--skip-secret-inspection` to the Job or CronJob args if Secret reads are not allowed in your cluster. Without Secret access, the scanner still checks ConfigMaps and attributes referenced ConfigMaps to applications.

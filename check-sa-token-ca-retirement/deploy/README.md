# Kubernetes Deployment

These manifests run `check-sa-token-ca-retirement` as a one-shot `Job` or as a scheduled `CronJob`.

Apply the default resources:

```bash
kubectl apply -k deploy
```

The manifests create a dedicated `check-sa-token-ca-retirement` namespace.

Run an ad-hoc scan from the CronJob template:

```bash
kubectl create job -n check-sa-token-ca-retirement --from=cronjob/check-sa-token-ca-retirement check-sa-token-ca-retirement-manual
kubectl logs -n check-sa-token-ca-retirement job/check-sa-token-ca-retirement-manual
```

Job and CronJob logs use the scanner's default CSV output:

```text
namespace,ownerKind,ownerName,serviceAccounts,issueTypes,maxBlockingTokenExpiration,legacyTokenSecrets,pods
```

Delete the one-shot Job before rerunning it with the same name:

```bash
kubectl delete job -n check-sa-token-ca-retirement check-sa-token-ca-retirement
kubectl apply -k deploy
```

The default image is:

```text
ghcr.io/dinoallo/check-sa-token-ca-retirement:latest
```

Override it with Kustomize:

```bash
kustomize edit set image ghcr.io/dinoallo/check-sa-token-ca-retirement=ghcr.io/YOUR_ORG/check-sa-token-ca-retirement:TAG
```

## Enabling CA and projected token checks

The default manifests report legacy ServiceAccount token Secrets and Pods still referencing them. To also check Pod `ca.crt` and current projected tokens, add args to `job.yaml` or `cronjob.yaml`, for example:

```yaml
args:
  - --new-ca-file=/etc/check-sa-token-ca-retirement/new-ca.pem
  - --sa-key-cutover=2026-06-08T08:00:00Z
  - --output=json
```

Mount the `new-ca.pem` file into the scanner Pod with your preferred ConfigMap or Secret mechanism. The file must contain the new CA certificate material in PEM format.

## RBAC

The RBAC includes:

- `list` on `pods`, `serviceaccounts`, and `secrets`
- `create` on `pods/exec`

`pods/exec` is only needed when `--new-ca-file` or `--sa-key-cutover` is used. Remove the `pods/exec` rule when the scanner is only checking legacy token Secrets. Remove `secrets` and add `--skip-legacy-secret-inspection` if Secret reads are not allowed; legacy Secret token findings will be skipped.

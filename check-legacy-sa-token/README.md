# check-legacy-sa-token

Scan Kubernetes Pods and report applications that reference legacy Secret-backed ServiceAccount tokens. An application is grouped by the Pod ownerReference, using the same definition as `check-kubeconfig-in-config`: the scanner uses the controller ownerReference when present, falls back to the first ownerReference, and reports standalone Pods as `Pod/<name>`.

The scanner treats a Secret as a legacy ServiceAccount token when `type` is `kubernetes.io/service-account-token`. It then reports Pods that reference those Secrets through:

- Secret volumes
- projected volume Secret sources
- `envFrom` Secret references
- individual environment variable Secret key references

## Build

```bash
go build -o check-legacy-sa-token .
```

## Usage

```bash
./check-legacy-sa-token
./check-legacy-sa-token --namespace prod
./check-legacy-sa-token --context my-cluster --output json --include-pods
```

Default output is CSV with these columns:

```text
namespace,ownerKind,ownerName,serviceAccounts,tokenServiceAccounts,tokenSecrets
```

Use `--output table` for a wider human-readable table. JSON output includes `unreferencedTokenSecrets` for legacy ServiceAccount token Secrets that were found but not referenced by any Pod seen during the scan.

Options:

- `--kubeconfig` kubeconfig path; falls back to in-cluster config when unavailable
- `--context` kubeconfig context to use
- `--namespace` namespace to scan; empty means all namespaces
- `--output` output format: `csv`, `table`, or `json`
- `--include-pods` include per-Pod details in JSON output
- `--max-samples` maximum Pod names to show per application in table output

## Kubernetes

Run the scanner in-cluster as a one-shot Job:

```bash
kubectl apply -k deploy
kubectl logs -n check-legacy-sa-token job/check-legacy-sa-token
```

The same deployment directory also includes a CronJob that runs daily at 02:00 cluster time:

```bash
kubectl get cronjob -n check-legacy-sa-token check-legacy-sa-token
```

See [deploy/README.md](deploy/README.md) for image override and RBAC notes.

## RBAC

Minimum read permissions:

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: check-legacy-sa-token
rules:
  - apiGroups: [""]
    resources: ["pods", "secrets"]
    verbs: ["list"]
```

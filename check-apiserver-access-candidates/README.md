# check-apiserver-access-candidates

Scan Kubernetes workloads for static signals that they may access kube-apiserver. This is intended to build a restart candidate list during Kubernetes root CA rotation, where long-running in-cluster clients may need to restart or rebuild their client-go transport to reload `ca.crt`.

This tool does not prove runtime traffic. Use audit logs or network flow logs for confirmed access. It classifies static candidates as:

- `likely`: the Pod has an explicit projected ServiceAccount token, references a kubeconfig ConfigMap/Secret, or uses an automounted ServiceAccount token whose ServiceAccount has RBAC bindings.
- `possible`: the Pod has an effective automounted ServiceAccount token, but no stronger static signal was found.

Applications are grouped by Pod ownerReference. The scanner uses the controller ownerReference when present, falls back to the first ownerReference, and reports standalone Pods as `Pod/<name>`.

## Build

```bash
go build -o check-apiserver-access-candidates .
```

## Usage

```bash
./check-apiserver-access-candidates
./check-apiserver-access-candidates --namespace prod
./check-apiserver-access-candidates --output json --include-pods
```

Options:

- `--kubeconfig` kubeconfig path; falls back to in-cluster config when unavailable
- `--context` kubeconfig context to use
- `--namespace` namespace to scan; empty means all namespaces
- `--output` output format: `csv`, `table`, or `json`
- `--include-pods` include per-Pod details in JSON output
- `--max-samples` maximum Pod names to show per application in table output
- `--skip-secret-inspection` skip listing Secrets for kubeconfig detection

## Signals

The scanner checks:

- Pod effective `automountServiceAccountToken`
- explicit projected `serviceAccountToken` volumes
- RoleBinding and ClusterRoleBinding subjects that reference ServiceAccounts
- ConfigMaps and Secrets containing kubeconfig data
- Pod references to those kubeconfig ConfigMaps or Secrets through volumes, projected volumes, `envFrom`, and individual environment variables

Default output is CSV with these columns:

```text
namespace,ownerKind,ownerName,confidence,serviceAccounts,reasons,pods
```

Use `--output table` for a wider human-readable table. JSON output includes `unreferencedResources` for kubeconfig ConfigMaps or Secrets that exist but were not referenced by any Pod seen during the scan.

## Kubernetes

Run the scanner in-cluster as a one-shot Job:

```bash
kubectl apply -k deploy
kubectl logs -n check-apiserver-access-candidates job/check-apiserver-access-candidates
```

The same deployment directory also includes a CronJob that runs daily at 02:00 cluster time:

```bash
kubectl get cronjob -n check-apiserver-access-candidates check-apiserver-access-candidates
```

See [deploy/README.md](deploy/README.md) for image override and RBAC notes.

## RBAC

Minimum read permissions:

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: check-apiserver-access-candidates
rules:
  - apiGroups: [""]
    resources: ["pods", "serviceaccounts", "configmaps", "secrets"]
    verbs: ["list"]
  - apiGroups: ["rbac.authorization.k8s.io"]
    resources: ["rolebindings", "clusterrolebindings"]
    verbs: ["list"]
```

Use `--skip-secret-inspection` when the kubeconfig should not read Secrets. Without Secret access, Secret-backed kubeconfig candidates are skipped.

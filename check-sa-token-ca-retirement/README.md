# check-sa-token-ca-retirement

Scan Kubernetes workloads for blockers before removing old root CA and old ServiceAccount public key material after a root CA / `sa.key` rotation.

The scanner follows the retirement checks from the rotation runbook:

- Pod `ca.crt` should contain the required new CA certificate material.
- Projected ServiceAccount tokens should have `iat` later than the new signing key cutover time, or old tokens should already be expired.
- Legacy `kubernetes.io/service-account-token` Secrets should not still be referenced by Pods, because these tokens usually do not expire naturally.

Applications are grouped by Pod ownerReference. The scanner uses the controller ownerReference when present, falls back to the first ownerReference, and reports standalone Pods as `Pod/<name>`.

## Build

```bash
go build -o check-sa-token-ca-retirement .
```

## Usage

```bash
./check-sa-token-ca-retirement
./check-sa-token-ca-retirement --namespace prod
./check-sa-token-ca-retirement --output json --include-pods
./check-sa-token-ca-retirement --sa-key-cutover 2026-06-08T08:00:00Z
./check-sa-token-ca-retirement --new-ca-file ./new-ca.pem --sa-key-cutover 2026-06-08T08:00:00Z
```

By default, the scanner lists Pods, ServiceAccounts, and legacy ServiceAccount token Secrets. Pod file inspection is enabled only when `--new-ca-file` or `--sa-key-cutover` is provided, because those checks require `pods/exec`.

Options:

- `--kubeconfig` kubeconfig path; falls back to in-cluster config when unavailable
- `--context` kubeconfig context to use
- `--namespace` namespace to scan; empty means all namespaces
- `--output` output format: `csv`, `table`, or `json`
- `--include-pods` include per-Pod details in JSON output
- `--max-samples` maximum Pod names to show per application in table output
- `--new-ca-file` PEM file containing the required new CA certificate material
- `--sa-key-cutover` RFC3339 time when the new ServiceAccount signing key started issuing tokens
- `--ca-cert-path` path to `ca.crt` inside containers, default `/var/run/secrets/kubernetes.io/serviceaccount/ca.crt`
- `--token-path` path to the default projected token inside containers, default `/var/run/secrets/kubernetes.io/serviceaccount/token`
- `--skip-pod-exec` skip Pod file checks even when CA or cutover options are set
- `--skip-legacy-secret-inspection` skip listing Secrets

## Output

Default output is CSV with these columns:

```text
namespace,ownerKind,ownerName,serviceAccounts,issueTypes,maxBlockingTokenExpiration,legacyTokenSecrets,pods
```

Issue types include:

- `ca_crt_missing_required_ca`
- `ca_crt_read_failed`
- `ca_crt_parse_failed`
- `projected_token_issued_before_cutover`
- `projected_token_read_failed`
- `projected_token_decode_failed`
- `projected_token_missing_iat`
- `legacy_secret_token_in_use`
- `pod_not_inspectable`

Use `--output table` for a wider human-readable table. JSON output includes `unreferencedLegacyTokenSecrets` for legacy ServiceAccount token Secrets that exist but were not referenced by any Pod seen during the scan.

## Kubernetes

Run the scanner in-cluster as a one-shot Job:

```bash
kubectl apply -k deploy
kubectl logs -n check-sa-token-ca-retirement job/check-sa-token-ca-retirement
```

The same deployment directory also includes a CronJob that runs daily at 02:00 cluster time:

```bash
kubectl get cronjob -n check-sa-token-ca-retirement check-sa-token-ca-retirement
```

See [deploy/README.md](deploy/README.md) for image override, CA bundle mounting, and RBAC notes.

## RBAC

Minimum read permissions for legacy token checks:

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: check-sa-token-ca-retirement
rules:
  - apiGroups: [""]
    resources: ["pods", "serviceaccounts", "secrets"]
    verbs: ["list"]
```

Add `pods/exec` when using `--new-ca-file` or `--sa-key-cutover`:

```yaml
- apiGroups: [""]
  resources: ["pods/exec"]
  verbs: ["create"]
```

Use `--skip-legacy-secret-inspection` when the kubeconfig should not read Secrets. Without Secret access, legacy Secret token findings are skipped.

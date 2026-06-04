# check-non-default-sa-token

Scan Kubernetes Pods and report applications whose Pods use a non-default ServiceAccount token. An application is grouped by the Pod ownerReference. The scanner uses the controller ownerReference when present, falls back to the first ownerReference, and reports standalone Pods as `Pod/<name>`.

## Build

```bash
go build -o check-non-default-sa-token .
```

## Usage

```bash
./check-non-default-sa-token
./check-non-default-sa-token --namespace prod
./check-non-default-sa-token --context my-cluster --output json --include-pods
```

## Kubernetes

Run the scanner in-cluster as a one-shot Job:

```bash
kubectl apply -k deploy
kubectl logs -n check-non-default-sa-token job/check-non-default-sa-token
```

The same deployment directory also includes a CronJob that runs daily at 02:00 cluster time:

```bash
kubectl get cronjob -n check-non-default-sa-token check-non-default-sa-token
```

See [deploy/README.md](deploy/README.md) for image override and RBAC notes.

The scanner checks:

- Pods whose `serviceAccountName` is not `default` and whose effective `automountServiceAccountToken` is enabled.
- Pods with projected `serviceAccountToken` volumes for a non-default Pod ServiceAccount.
- Legacy `kubernetes.io/service-account-token` Secret volumes when Secret inspection is permitted.

Use `--skip-secret-inspection` when the kubeconfig should not read Secrets. Without Secret access, the scanner still detects normal non-default ServiceAccount token use from Pod and ServiceAccount specs, but may miss manually mounted legacy token Secrets.

## RBAC

Minimum read permissions for normal detection:

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: check-non-default-sa-token
rules:
  - apiGroups: [""]
    resources: ["pods", "serviceaccounts"]
    verbs: ["list"]
```

Add this rule for legacy Secret token volume attribution:

```yaml
- apiGroups: [""]
  resources: ["secrets"]
  verbs: ["list"]
```

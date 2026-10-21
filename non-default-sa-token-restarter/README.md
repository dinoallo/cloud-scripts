# non-default-sa-token-restarter

Find active Kubernetes Pods scheduled on a node that use a non-`default`
ServiceAccount token, then optionally delete those Pods so their controllers
recreate them.

The tool detects the following token paths:

- a non-`default` Pod ServiceAccount with effective
  `automountServiceAccountToken=true`;
- an explicit projected `serviceAccountToken` volume; and
- a legacy `kubernetes.io/service-account-token` Secret referenced by a volume,
  projected volume, or environment variable.

Dry-run is the default. The tool skips unscheduled, terminating, and terminal
(`Succeeded` or `Failed`) Pods. It does not require Pods to have a controller;
deleting an unmanaged Pod is permanent.

## Build

```bash
go build -o non-default-sa-token-restarter .
```

## Usage

List matching Pods on every node:

```bash
./non-default-sa-token-restarter
```

Limit the scan to one node and namespace:

```bash
./non-default-sa-token-restarter --node worker-1 --namespace prod
```

After reviewing the dry-run output, delete the matching Pods:

```bash
./non-default-sa-token-restarter --node worker-1 --confirm
```

Options:

- `--kubeconfig` kubeconfig path; falls back to in-cluster config when unavailable
- `--context` kubeconfig context to use
- `--namespace` namespace to scan; empty means all namespaces
- `--node` exact node name to scan; empty means all scheduled nodes
- `--exclude-pod` Pod to skip in `namespace/name` form; when `POD_NAMESPACE` and
  `POD_NAME` are set, their Pod is excluded by default
- `--confirm` delete matching Pods; without it, only print the deletion plan
- `--skip-secret-inspection` do not list Secrets; legacy token Secret references
  may be missed

## Kubernetes Job

The `deploy/` directory contains a one-shot Job, a dedicated ServiceAccount,
and the minimum ClusterRole permissions needed to list Pods, ServiceAccounts,
and Secrets and delete Pods. The Job is dry-run by default:

```bash
kubectl apply -k deploy
kubectl logs -n non-default-sa-token-restarter job/non-default-sa-token-restarter
```

The Job uses the Downward API to set `POD_NAMESPACE` and `POD_NAME`, so it does
not select its own Pod if `--confirm` is later added to the container args.

To perform deletion, add `--confirm` to `deploy/job.yaml` only after reviewing
the dry-run output. Override the image with Kustomize when needed.

## Safety and permissions

- No Pod is deleted unless `--confirm` is supplied.
- `--node` is an exact `spec.nodeName` filter; the default scans all scheduled
  nodes.
- If Secret inspection is unavailable, the program warns and continues with
  direct Pod and ServiceAccount detection. Use `--skip-secret-inspection` when
  Secret access is intentionally unavailable.
- The runtime identity needs `list` on Pods, ServiceAccounts, and Secrets, plus
  `delete` on Pods. Secret `list` is not required with
  `--skip-secret-inspection`.

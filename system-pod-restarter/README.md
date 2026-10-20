# system-pod-restarter

Find Pods owned by Deployments and DaemonSets in every namespace whose name does
not start with `ns-`, then optionally delete those Pods so their controllers
recreate them.

The tool follows Kubernetes owner references. Deployment Pods are found through
their ReplicaSet owner; Pods owned by StatefulSets, Jobs, or no controller are
not selected.

## Build

```bash
go build -o system-pod-restarter .
```

## Usage

Dry-run is the default:

```bash
./system-pod-restarter
```

Delete the matching Pods:

```bash
./system-pod-restarter --confirm
```

Use a different namespace exclusion prefix when needed:

```bash
./system-pod-restarter --exclude-namespace-prefix platform- --confirm
```

Options:

- `--kubeconfig` kubeconfig path; falls back to in-cluster config when unavailable
- `--context` kubeconfig context to use
- `--exclude-namespace-prefix` skip namespaces whose names start with this prefix; defaults to `ns-`
- `--confirm` delete selected Pods; without it, only print the deletion plan

## Kubernetes Job

The `deploy/` directory contains a one-shot Job and the minimum ClusterRole
permissions required to list namespaces, Deployments, DaemonSets, ReplicaSets,
and Pods, and to delete Pods. The Job is dry-run by default. Add `--confirm` to
the container arguments in `deploy/job.yaml` only after reviewing the dry-run
output.

```bash
kubectl apply -k deploy
kubectl logs -n system-pod-restarter job/system-pod-restarter
```

## Safety and permissions

- Namespaces beginning with `ns-` are excluded by default.
- Only Pods owned by a Deployment or DaemonSet are selected.
- The default mode never deletes anything; `--confirm` is required.
- Deleting a Pod does not delete its Deployment, DaemonSet, ReplicaSet, or namespace.
- The runtime identity needs `list` on namespaces, Pods, Deployments, DaemonSets,
  and ReplicaSets, plus `delete` on Pods.

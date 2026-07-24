# pvc-cleaner

Delete explicitly reviewed PVCs from a `namespace,pvc` target list.

`pvc-cleaner` does not scan the cluster and does not decide whether a PVC is a
leftover object. Use `template-pvc-scanner` first, review and reduce its CSV
output, then pass the reviewed list to this cleaner.

## Build

```bash
go build -o pvc-cleaner .
```

## Input

The input can be either a minimal CSV:

```csv
namespace,pvc
prod,data-mysql-0
prod,data-redis-0
```

Or the CSV produced by `template-pvc-scanner`; `pvc-cleaner` reads only the
`namespace` and `pvc` columns and ignores the rest:

```csv
namespace,pvc,path,value,reason,pvcPhase,pvcStorageClass,pvcSize,pvcAge
prod,data-mysql-0,/data,10,reviewed,Bound,fast,10Gi,2d1h
```

Headerless input is also accepted as either `namespace,pvc` or `namespace/pvc`
per line. Duplicate PVC targets are ignored after the first occurrence.

## Usage

Dry-run is the default. It reads the target list and checks whether each PVC
currently exists:

```bash
./pvc-cleaner --input reviewed-pvcs.csv
```

Delete the reviewed PVCs:

```bash
./pvc-cleaner --input reviewed-pvcs.csv --confirm
```

Read from stdin:

```bash
cat reviewed-pvcs.csv | ./pvc-cleaner --input -
```

## Kubernetes Job

The `deploy/` directory contains a Job that mounts a reviewed CSV from a
ConfigMap at `/input/targets.csv`. Replace `deploy/targets.example.csv` with the
reviewed target list before applying the Job.

Apply the dry-run Job:

```bash
kubectl apply -k deploy
kubectl -n pvc-cleaner logs job/pvc-cleaner -c cleaner
```

To perform deletion, edit `deploy/job.yaml` and add `--confirm` to the container
args after `--input /input/targets.csv`, then apply the Job again.

## Permissions

The Job RBAC grants only these PVC permissions:

```text
resources: persistentvolumeclaims
verbs: get, delete
```

It does not grant `list` on PVCs or Pods, and it does not grant any PV
permission. After a PVC is deleted, whether its bound PV is deleted or retained
is controlled by the PV reclaim policy.

For stricter production execution, split the reviewed CSV by namespace and bind
namespace-scoped Roles instead of the provided ClusterRole.

## Safety Rules

- The tool never discovers targets by itself.
- `--confirm` is required for deletion.
- Missing PVCs are reported as `status=not-found` and skipped.
- PVs are never listed, read, or deleted by this tool.

## Options

- `--kubeconfig` kubeconfig path; falls back to in-cluster config when available
- `--input` target CSV path; use `-` for stdin
- `--confirm` delete listed PVCs; without it, only print a dry-run plan
- `--wait` maximum time to wait for each PVC deletion

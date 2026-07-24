# orphaned-pvc-scanner

Scan Kubernetes PVCs that have no ownerReferences and are not currently used by
active Pods. This tool is read-only. It does not delete PVCs or PVs.

This scanner is broader than `template-pvc-scanner`: it does not require a
template instance, StatefulSet name, or claim template. By default, it only uses
PVC metadata, bound PV metadata, and Pod volume references. It does not resolve
ownerReferences or query arbitrary owner resource types unless `--resolve-owners`
is explicitly set.

## Build

```bash
go build -o orphaned-pvc-scanner .
```

## Usage

By default, the scanner checks all namespaces:

```bash
./orphaned-pvc-scanner --output csv > orphaned-pvcs.csv
```

Limit the scan to one namespace:

```bash
./orphaned-pvc-scanner --namespace prod --output table
```

Use JSON Lines for follow-up automation:

```bash
./orphaned-pvc-scanner --output jsonl > orphaned-pvcs.jsonl
```

Enable deep ownerReference resolution when the scanner has read permissions for
the relevant owner resource types:

```bash
./orphaned-pvc-scanner --resolve-owners --output csv > orphaned-pvcs.csv
```

## Owner Status

The scanner reports one row per PVC candidate. By default, `ownerStatus` can be:

- `noOwnerReferences`: the PVC has no ownerReferences and is not referenced by
  any non-terminal Pod.

When `--resolve-owners` is set, these additional statuses can also be reported:

- `ownerNotFound`: the referenced owner object was not found.
- `ownerUIDMismatch`: an object with the referenced name exists, but its UID
  does not match the ownerReference UID.
- `ownerGVKNotFound`: the ownerReference apiVersion/kind is not available from
  Kubernetes discovery.
- `ownerInvalidScope`: the referenced namespaced owner UID was found in a
  different namespace. A PVC can only point to same-namespace namespaced owners
  or cluster-scoped owners.
- `ownerLookupError`: the scanner could not verify the owner because lookup
  failed, usually due to RBAC or API errors. Treat this as unknown, not as a
  deletion signal.

If any ownerReference resolves to an existing owner object with the matching
UID, the PVC is not reported. Without `--resolve-owners`, PVCs that have
ownerReferences are skipped without querying their owners.

For PVCs with no ownerReferences, the scanner lists Pods in the scan scope and
skips PVCs referenced by Pods that are not in `Succeeded` or `Failed` phase.
This filter applies in both default and `--resolve-owners` modes.

## Output

CSV and table output use these review-focused fields:

```text
namespace,pvc,pv,ownerStatus,reason,pvcPhase,pvPhase,pvReclaimPolicy,pvClaimRefMatched,pvcStorageClass
```

The PV fields are populated when the PVC has `spec.volumeName` and the bound PV
is present in the cluster PV list. The scanner lists PVs once and matches them
locally, instead of issuing one PV request per candidate PVC. `pvClaimRefMatched`
shows whether the PV still points back to the candidate PVC.

JSON Lines output keeps the full row object, including ownerReference details,
PVC size, and PVC age, for follow-up automation.

## Safety Notes

`noOwnerReferences` is a low-confidence signal. Many valid PVCs are intentionally
created without ownerReferences, including manually created PVCs and PVCs
managed by deployment tools that do not set ownerReferences.
To reduce noise, the scanner does not report no-owner PVCs that are still
referenced by active Pods.

`ownerNotFound`, `ownerUIDMismatch`, `ownerGVKNotFound`, and `ownerInvalidScope`
are stronger orphan signals available only with `--resolve-owners`, but they
should still be reviewed with workload context before deletion.

## Kubernetes Job

Run the scanner in-cluster as a one-shot Job:

```bash
kubectl apply -k deploy
kubectl logs -n orphaned-pvc-scanner job/orphaned-pvc-scanner
```

The Job writes CSV to stdout. Redirect logs locally when you want a review file:

```bash
kubectl logs -n orphaned-pvc-scanner job/orphaned-pvc-scanner > orphaned-pvcs.csv
```

The default RBAC is read-only and intentionally minimal. It can list PVCs, list
PVs, and list Pods to avoid reporting no-owner PVCs that are still in use.
It does not grant discovery or owner resource permissions. If you enable
`--resolve-owners`, grant this ServiceAccount additional read-only discovery and
owner resource `get`/`list` permissions for the resource types you want to
verify.

## Options

- `--kubeconfig` kubeconfig path; falls back to in-cluster config when available
- `--namespace` namespace to scan; empty means all namespaces
- `--output` output format: `table`, `csv`, or `jsonl`; default is `table`
- `--resolve-owners` resolve ownerReferences by querying owner objects; default
  is disabled

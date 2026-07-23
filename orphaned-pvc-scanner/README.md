# orphaned-pvc-scanner

Scan Kubernetes PVCs whose owner relationship is missing, broken, or cannot be
verified. This tool is read-only. It does not delete PVCs or PVs.

This scanner is broader than `template-pvc-scanner`: it does not require a
template instance, StatefulSet name, or claim template. It reports PVCs with no
ownerReferences, unless they are referenced by a currently active Pod, and PVCs
whose ownerReferences do not resolve to an existing owner object with the
referenced UID.

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

## Owner Status

The scanner reports one row per PVC candidate. `ownerStatus` is one of:

- `noOwnerReferences`: the PVC has no ownerReferences and is not referenced by
  any non-terminal Pod.
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
UID, the PVC is not reported.

For PVCs with no ownerReferences, the scanner lists Pods in the scan scope and
skips PVCs referenced by Pods that are not in `Succeeded` or `Failed` phase.
This filter only applies to `noOwnerReferences`; PVCs with broken ownerReferences
are still reported even if a Pod references them.

## Output

Structured output uses these fields:

```text
namespace,pvc,pv,ownerStatus,ownerAPIVersion,ownerKind,ownerNamespace,ownerName,ownerUID,ownerController,ownerBlockOwnerDeletion,ownerRefCount,reason,pvcPhase,pvPhase,pvReclaimPolicy,pvClaimRefMatched,pvcStorageClass,pvcSize,pvcAge
```

The PV fields are populated when the PVC has `spec.volumeName` and the bound PV
can be read. `pvClaimRefMatched` shows whether the PV still points back to the
candidate PVC.

## Safety Notes

`noOwnerReferences` is a low-confidence signal. Many valid PVCs are intentionally
created without ownerReferences, including manually created PVCs and PVCs
managed by deployment tools that do not set ownerReferences.
To reduce noise, the scanner does not report no-owner PVCs that are still
referenced by active Pods.

`ownerNotFound`, `ownerUIDMismatch`, `ownerGVKNotFound`, and `ownerInvalidScope`
are stronger orphan signals, but they should still be reviewed with workload
context before deletion.

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

The default RBAC is read-only. It can verify common workload owners such as
Pods, ReplicationControllers, Deployments, ReplicaSets, StatefulSets,
DaemonSets, Jobs, and CronJobs. If PVCs are owned by CRDs or other resource
types, grant this ServiceAccount additional read-only `get`/`list` permissions
for those owner resources; otherwise those PVCs may be reported as
`ownerLookupError`.

## Options

- `--kubeconfig` kubeconfig path; falls back to in-cluster config when available
- `--namespace` namespace to scan; empty means all namespaces
- `--output` output format: `table`, `csv`, or `jsonl`; default is `table`

# template-pvc-scanner

Scan for PVC/PV candidates left by the Sealos template frontend PVC deletion
bug. This is the read-only companion to `template-pvc-cleaner`.

The affected template frontend version can create a StatefulSet with
`cloud.sealos.io/deploy-on-sealos=<instance>`, while the StatefulSet
`volumeClaimTemplates` do not copy that label to generated PVCs. Instance
deletion logic that deletes PVCs by the instance label then misses those
StatefulSet PVCs. The StatefulSet may be removed by garbage collection, while
the PVC and its bound PV remain.

This scanner has no delete path and no `--confirm` option. It only lists
candidates and checks whether a bound PV still points to the candidate PVC.

## Build

```bash
go build -o template-pvc-scanner .
```

## Kubernetes Job

The `deploy/` directory contains a read-only Job setup that writes CSV output to
`/scan-output/pvc-scan.csv` inside the pod.

Before applying it, edit `deploy/job.yaml`:

- set `TEMPLATE_INSTANCE`
- adjust `SCANNER_ARGS` if you want `--namespace`, `--statefulset`, or
  `--claim-template`
- replace `ghcr.io/dinoallo/template-pvc-scanner:latest` if you build and push
  the image under a different registry or owner

The default image is built from `template-pvc-scanner/Dockerfile` and includes
the scanner binary at `/template-pvc-scanner`, `/bin/sh` for the wrapper
command, and `tar` for `kubectl cp`.

Apply the Job:

```bash
kubectl apply -k deploy
```

Wait until the log says the CSV was written:

```bash
kubectl -n template-pvc-scanner logs job/template-pvc-scanner -c scanner
```

The Job keeps the pod running after the scan because `kubectl cp` needs to exec
into a live container. Copy the CSV, then delete the Job:

```bash
pod="$(kubectl -n template-pvc-scanner get pod -l job-name=template-pvc-scanner -o jsonpath='{.items[0].metadata.name}')"
kubectl -n template-pvc-scanner cp "$pod:/scan-output/pvc-scan.csv" ./pvc-scan.csv -c scanner
kubectl -n template-pvc-scanner delete job template-pvc-scanner
```

The RBAC in `deploy/rbac.yaml` is read-only and only grants access to PVCs, PVs,
and StatefulSets.

## Usage

By default, the scanner checks all namespaces. Add `--namespace prod` to limit
the scan to one namespace.

When the StatefulSet still exists, scan live StatefulSets with the template
instance label:

```bash
./template-pvc-scanner \
  --instance my-template-instance
```

When the StatefulSet has already been deleted but the StatefulSet name and
volumeClaimTemplate name are known, provide them explicitly:

```bash
./template-pvc-scanner \
  --instance my-template-instance \
  --statefulset mysql \
  --claim-template data
```

When the StatefulSet has already been deleted and its name is unknown, use
conservative orphan discovery:

```bash
./template-pvc-scanner \
  --instance my-template-instance \
  --discover-orphans
```

The output includes the PVC, inferred StatefulSet, claim template, reason, bound
PV, PV phase, reclaim policy, and whether the PV `claimRef` matches the PVC.

The default output is a table for reading Job logs:

```bash
./template-pvc-scanner \
  --instance my-template-instance \
  --output table
```

For production review, CSV is easier to sort, filter, and annotate in a
spreadsheet:

```bash
./template-pvc-scanner \
  --instance my-template-instance \
  --discover-orphans \
  --output csv > pvc-scan.csv
```

For follow-up automation, JSON Lines emits one candidate per line:

```bash
./template-pvc-scanner \
  --instance my-template-instance \
  --discover-orphans \
  --output jsonl > pvc-scan.jsonl
```

Structured output uses these fields:

```text
namespace,pvc,pv,statefulset,claimTemplate,reason,pvcPhase,pvPhase,pvReclaimPolicy,pvClaimRefMatched,pvcStorageClass,pvcSize,pvcAge
```

## Safety Rules

The scanner scopes every run to a single template instance. Namespace is
optional; when omitted, the scanner checks all namespaces and keeps StatefulSet
and PVC matching namespace-aware.

It only treats a PVC as affected when the PVC has no
`cloud.sealos.io/deploy-on-sealos` label. PVCs that already carry that label are
not reported.

Live StatefulSet mode requires:

- a StatefulSet selected by `cloud.sealos.io/deploy-on-sealos=<instance>`
- a `volumeClaimTemplate` that lacks that same label
- a PVC name matching `<claim-template>-<statefulset>-<ordinal>`

Explicit orphan mode requires:

- `--statefulset` and `--claim-template`
- a PVC name matching `<claim-template>-<statefulset>-<ordinal>`
- when the StatefulSet is gone, legacy `app=<statefulset>` or `app=<instance>`
  evidence

`--discover-orphans` mode requires:

- no `cloud.sealos.io/deploy-on-sealos` label on the PVC
- legacy `app=<statefulset>` label on the PVC
- PVC name matching `<claim-template>-<statefulset>-<ordinal>`
- no live StatefulSet with that `app` name in the same namespace

## Options

- `--kubeconfig` kubeconfig path; falls back to in-cluster config when available
- `--namespace` namespace to scan; empty means all namespaces
- `--instance` template instance name
- `--output` output format: `table`, `csv`, or `jsonl`; default is `table`
- `--statefulset` StatefulSet name to inspect; repeat for multiple values
- `--claim-template` volumeClaimTemplate name; repeat for multiple values
- `--discover-orphans` discover orphan StatefulSet PVCs from legacy `app` labels

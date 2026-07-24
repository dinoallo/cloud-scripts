# template-pvc-scanner

Scan for PVC/PV candidates left by the Sealos template/applaunchpad PVC
deletion bug. This is the read-only companion to `template-pvc-cleaner`.

Affected local-store PVCs are generated from StatefulSet
`volumeClaimTemplates` that carry `path` and `value` annotations, but miss the
labels used by the delete path. When the app or template instance is deleted,
the StatefulSet may be removed while its PVC and bound PV remain.

This scanner does not inspect StatefulSets and does not need a StatefulSet or
template instance name. It only lists candidates and checks whether a bound PV
still points to the candidate PVC.

## Build

```bash
go build -o template-pvc-scanner .
```

## Kubernetes Job

The `deploy/` directory contains a read-only Job setup that writes CSV output to
`/scan-output/pvc-scan.csv` inside the pod.

Before applying it, edit `deploy/job.yaml` if needed:

- set `SCANNER_ARGS` to add options such as `--namespace prod`; leave it empty
  to scan all namespaces
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

The RBAC in `deploy/rbac.yaml` is read-only and only grants list access to PVCs
and Pods, plus get access to PVs.

## Usage

By default, the scanner checks all namespaces:

```bash
./template-pvc-scanner --output csv > pvc-scan.csv
```

Add `--namespace prod` to limit the scan to one namespace:

```bash
./template-pvc-scanner \
  --namespace prod \
  --output table
```

For follow-up automation, JSON Lines emits one candidate per line:

```bash
./template-pvc-scanner --output jsonl > pvc-scan.jsonl
```

Structured output uses these fields:

```text
namespace,pvc,pv,path,value,reason,pvcPhase,pvPhase,pvReclaimPolicy,pvClaimRefMatched,pvcStorageClass,pvcSize,pvcAge
```

## Safety Rules

The scanner is PVC-only. It never lists or matches StatefulSets.

It reports a PVC only when all of these are true:

- the PVC has non-empty `path` and `value` annotations
- the PVC has none of these ownership labels: `cloud.sealos.io/deploy-on-sealos`,
  `cloud.sealos.io/app-deploy-manager`, `app`
- no active Pod references the PVC in the same namespace

Pods in `Succeeded` or `Failed` phase are treated as terminal and do not block a
candidate. Pending, Running, Unknown, and not-yet-phased Pods block the PVC.

PV reporting is guarded. A bound PV is included only after checking whether its
`claimRef` still points to the candidate PVC namespace, name, and UID.

## Options

- `--kubeconfig` kubeconfig path; falls back to in-cluster config when available
- `--namespace` namespace to scan; empty means all namespaces
- `--output` output format: `table`, `csv`, or `jsonl`; default is `table`

Deprecated scope flags `--instance`, `--statefulset`, `--claim-template`, and
`--discover-orphans` are accepted for old wrapper compatibility but ignored.

# template-pvc-cleaner

Clean up PVC/PV objects left by the Sealos template/applaunchpad PVC deletion
bug.

Affected local-store PVCs are generated from StatefulSet
`volumeClaimTemplates` that carry `path` and `value` annotations, but miss the
labels used by the delete path. When the app or template instance is deleted,
the StatefulSet may be removed while its PVC and bound PV remain.

The cleaner does not inspect StatefulSets and does not need a StatefulSet or
template instance name. It is dry-run by default and only deletes after
`--confirm` is passed.

## Build

```bash
go build -o template-pvc-cleaner .
```

## Usage

Review one namespace first. Without `--confirm`, the cleaner only prints the
planned PVC/PV targets:

```bash
./template-pvc-cleaner --namespace prod
```

After reviewing the dry-run output, add `--confirm` to delete the matching PVCs
and their guarded leftover PVs:

```bash
./template-pvc-cleaner \
  --namespace prod \
  --confirm
```

Use `--delete-pv=false` if you only want to delete PVCs and leave PVs untouched:

```bash
./template-pvc-cleaner \
  --namespace prod \
  --delete-pv=false \
  --confirm
```

## Safety Rules

The cleaner always requires `--namespace`; it will not run across all namespaces
in one command.

It deletes a PVC only when all of these are true:

- the PVC has non-empty `path` and `value` annotations
- the PVC has none of these ownership labels: `cloud.sealos.io/deploy-on-sealos`,
  `cloud.sealos.io/app-deploy-manager`, `app`
- no active Pod references the PVC in the same namespace

Pods in `Succeeded` or `Failed` phase are treated as terminal and do not block a
candidate. Pending, Running, Unknown, and not-yet-phased Pods block deletion.

PV deletion is also guarded. A bound PV is deleted only when its `claimRef`
still points to the captured target PVC namespace, name, and UID. If the
`claimRef` changes, the cleaner refuses to delete that PV.

Deprecated scope flags `--instance`, `--statefulset`, `--claim-template`,
`--discover-orphans`, and `--allow-name-only` are rejected instead of ignored,
because they no longer narrow PVC-only cleanup.

## Options

- `--kubeconfig` kubeconfig path; falls back to in-cluster config when available
- `--namespace` namespace containing leftover template PVCs
- `--confirm` delete matching PVCs/PVs; without it, only print a dry-run plan
- `--delete-pv=false` delete only PVCs, leaving PVs untouched
- `--wait` maximum time to wait for each PVC/PV deletion

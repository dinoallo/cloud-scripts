# template-pvc-cleaner

Clean up PVC/PV objects left by the Sealos template frontend PVC deletion bug.

The affected template frontend version can create a StatefulSet with
`cloud.sealos.io/deploy-on-sealos=<instance>`, while the StatefulSet
`volumeClaimTemplates` do not copy that label to generated PVCs. Instance
deletion logic that deletes PVCs by the instance label then misses those
StatefulSet PVCs. The StatefulSet may be removed by garbage collection, while
the PVC and its bound PV remain.

The cleaner is dry-run by default. It only deletes after `--confirm` is passed.

## Build

```bash
go build -o template-pvc-cleaner .
```

## Usage

When the StatefulSet still exists, the cleaner can discover affected PVCs from
live StatefulSets with the template instance label:

```bash
./template-pvc-cleaner \
  --namespace prod \
  --instance my-template-instance
```

When the StatefulSet has already been deleted but the StatefulSet name and
volumeClaimTemplate name are known, provide them explicitly:

```bash
./template-pvc-cleaner \
  --namespace prod \
  --instance my-template-instance \
  --statefulset mysql \
  --claim-template data
```

When the StatefulSet has already been deleted and its name is unknown, use
conservative orphan discovery:

```bash
./template-pvc-cleaner \
  --namespace prod \
  --instance my-template-instance \
  --discover-orphans
```

Review the dry-run output first. Add `--confirm` only after the target list is
verified:

```bash
./template-pvc-cleaner \
  --namespace prod \
  --instance my-template-instance \
  --statefulset mysql \
  --claim-template data \
  --confirm
```

## Safety Rules

The cleaner scopes every run to a single namespace and template instance.

It only treats a PVC as affected when the PVC has no
`cloud.sealos.io/deploy-on-sealos` label. PVCs that already carry that label are
not deleted.

Live StatefulSet mode requires:

- a StatefulSet selected by `cloud.sealos.io/deploy-on-sealos=<instance>`
- a `volumeClaimTemplate` that lacks that same label
- a PVC name matching `<claim-template>-<statefulset>-<ordinal>`

Explicit orphan mode requires:

- `--statefulset` and `--claim-template`
- a PVC name matching `<claim-template>-<statefulset>-<ordinal>`
- by default, legacy `app=<statefulset>` or `app=<instance>` evidence

`--discover-orphans` mode requires:

- no `cloud.sealos.io/deploy-on-sealos` label on the PVC
- legacy `app=<statefulset>` label on the PVC
- PVC name matching `<claim-template>-<statefulset>-<ordinal>`
- no live StatefulSet with that `app` name

PV deletion is also guarded. A bound PV is deleted only when its `claimRef`
still points to the captured target PVC namespace, name, and UID.

Avoid `--allow-name-only` in production unless the target PVCs were manually
reviewed. It relaxes the legacy label evidence requirement for explicit orphan
cleanup.

## Options

- `--kubeconfig` kubeconfig path; falls back to in-cluster config when available
- `--namespace` namespace containing leftover template PVCs
- `--instance` template instance name
- `--statefulset` StatefulSet name to inspect; repeat for multiple values
- `--claim-template` volumeClaimTemplate name; repeat for multiple values
- `--discover-orphans` discover orphan StatefulSet PVCs from legacy `app` labels
- `--confirm` delete matching PVCs/PVs; without it, only print a dry-run plan
- `--delete-pv=false` delete only PVCs, leaving PVs untouched
- `--allow-name-only` allow explicit orphan cleanup without legacy label evidence
- `--wait` maximum time to wait for each PVC/PV deletion

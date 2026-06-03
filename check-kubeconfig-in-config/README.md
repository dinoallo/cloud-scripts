# check-kubeconfig-in-config

Scan Kubernetes ConfigMaps and Secrets for kubeconfig content, then report applications whose Pods reference those resources. An application is grouped by the Pod controller owner, with ReplicaSets promoted to Deployments and Jobs promoted to CronJobs.

The scanner treats a value as kubeconfig when it can be parsed by client-go as a kubeconfig and contains cluster and context entries. It checks:

- ConfigMap `data` and `binaryData`
- Secret `data` after Kubernetes client decoding
- base64-encoded kubeconfig values stored inside ConfigMaps or Secrets
- Pod references through volumes, projected volumes, `envFrom`, and individual environment variable references

## Build

```bash
go build -o check-kubeconfig-in-config .
```

## Usage

```bash
./check-kubeconfig-in-config
./check-kubeconfig-in-config --namespace prod
./check-kubeconfig-in-config --context my-cluster --output json --include-pods
```

Options:

- `--kubeconfig` kubeconfig path; falls back to in-cluster config when unavailable
- `--context` kubeconfig context to use
- `--namespace` namespace to scan; empty means all namespaces
- `--output` output format: `table` or `json`
- `--include-pods` include per-Pod details in JSON output
- `--max-samples` maximum Pod names to show per application in table output
- `--skip-secret-inspection` skip listing Secrets

## Output

Table output lists applications that reference ConfigMaps or Secrets containing kubeconfig content:

```text
NAMESPACE  OWNER           CONFIG_RESOURCES                       PODS  SAMPLE_PODS
prod       Deployment/api  ConfigMap/kubeconfigs[admin.conf:plain] 2     api-0,api-1
```

JSON output also includes `unreferencedResources` for ConfigMaps or Secrets that contain kubeconfig data but are not referenced by any Pod seen during the scan.

## RBAC

Minimum read permissions for application attribution:

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: check-kubeconfig-in-config
rules:
  - apiGroups: [""]
    resources: ["pods", "configmaps"]
    verbs: ["list"]
  - apiGroups: ["apps"]
    resources: ["replicasets"]
    verbs: ["list"]
  - apiGroups: ["batch"]
    resources: ["jobs"]
    verbs: ["list"]
```

Add this rule to inspect kubeconfig content stored in Secrets:

```yaml
- apiGroups: [""]
  resources: ["secrets"]
  verbs: ["list"]
```

Use `--skip-secret-inspection` when the kubeconfig should not read Secrets. Without Secret access, the scanner still detects kubeconfig content in ConfigMaps and attributes referenced ConfigMaps to applications, but Secret findings are skipped.

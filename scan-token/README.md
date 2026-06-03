# scan-token (Go)

Small CLI to scan Kubernetes for a leaked token. It checks:

- direct Pod environment variable values (`container.env.value` and init containers)
- ConfigMap text and binary data
- Secret data after Kubernetes client decoding, plus exact base64 equality

Requirements: a kubeconfig or in-cluster configuration with read access to the target namespace(s).

## Build

```bash
go build -o scan-token .
```

## Usage

```bash
./scan-token -t "my-secret-token"
./scan-token -f /path/to/token-file -n my-namespace
./scan-token -f /path/to/token-file -A
```

Options:

- `-t` token string to search for
- `-f` file containing the token on the first line
- `-n` namespace to scan; defaults to the current kubeconfig context namespace, then `default`
- `-A` scan all namespaces; overrides `-n`
- `-l` label selector for listed resources
- `-F` field selector for listed resources
- `-c` chunk size for list requests; default `500`
- `-w` number of concurrent workers; default `runtime.NumCPU() * 4`

Exit codes: `0` = no findings, `1` = found one or more occurrences, `2` = error.

#!/usr/bin/env bash
set -euo pipefail

# scan-token.bash
# Scans Kubernetes pods (env vars), ConfigMaps and Secrets for a leaked token.
# Requirements: kubectl, jq, base64

usage() {
	cat <<EOF
Usage: $0 -t TOKEN [-n NAMESPACE] | -f FILE [-n NAMESPACE]

Scans pods' environment variables, ConfigMaps and Secrets for the given TOKEN.

Options:
	-t TOKEN     Token string to search for
	-f FILE      File containing the token (first line)
	-n NAMESPACE Namespace to limit the scan (default: current context namespace)
	-A           Scan all namespaces (overrides -n)
	-l LABEL     Limit resources by label selector (e.g. -l app=web)
	-F FIELDSEL  Limit resources by field selector (e.g. -F metadata.name=my-pod)
	-c CHUNK     Chunk size for kubectl list requests (default: 500)
	-h           Show this help

Examples:
	$0 -t "secret-token"
	$0 -f leaked.txt -n default
EOF
}

if ! command -v kubectl >/dev/null 2>&1; then
	echo "kubectl is required but not found in PATH" >&2
	exit 2
fi

if ! command -v jq >/dev/null 2>&1; then
	echo "jq is required but not found in PATH" >&2
	exit 2
fi

NAMESPACE=""
ALL_NS=0
LABEL_SELECTOR=""
FIELD_SELECTOR=""
CHUNK=500
TOKEN=""

while getopts ":t:f:n:Al:F:c:h" opt; do
	case $opt in
		t) TOKEN="$OPTARG" ;;
		f) if [[ -r "$OPTARG" ]]; then TOKEN=$(sed -n '1p' "$OPTARG"); else echo "Cannot read file $OPTARG" >&2; exit 2; fi ;;
		n) NAMESPACE="$OPTARG" ;;
		A) ALL_NS=1 ;;
		l) LABEL_SELECTOR="$OPTARG" ;;
		F) FIELD_SELECTOR="$OPTARG" ;;
		c) CHUNK="$OPTARG" ;;
		h) usage; exit 0 ;;
		:) echo "Option -$OPTARG requires an argument." >&2; usage; exit 2 ;;
		\?) echo "Unknown option: -$OPTARG" >&2; usage; exit 2 ;;
	esac
done

if [[ -z "$TOKEN" ]]; then
	echo "A token must be provided with -t or -f" >&2
	usage
	exit 2
fi

TOKEN=$(printf '%s' "$TOKEN")
TOKEN_B64=$(printf '%s' "$TOKEN" | base64 | tr -d '\n')

mask() {
	local s="$1"
	local len=${#s}
	if (( len <= 8 )); then
		echo "********"
	else
		local pre=${s:0:4}
		local suf=${s: -4}
		echo "${pre}...${suf}"
	fi
}

FOUND=0

report() {
	FOUND=1
	echo "[FOUND] $*"
}

declare -a KUBE_ARGS
if [[ "$ALL_NS" -eq 1 ]]; then
	KUBE_ARGS=( --all-namespaces )
else
	if [[ -z "$NAMESPACE" ]]; then
		# try to detect current namespace from context, fallback to 'default'
		NAMESPACE=$(kubectl config view --minify --output 'jsonpath={..namespace}' 2>/dev/null || true)
		if [[ -z "$NAMESPACE" ]]; then
			NAMESPACE=default
		fi
	fi
	KUBE_ARGS=( -n "$NAMESPACE" )
fi

# add label/field selectors and chunk size safely
if [[ -n "$LABEL_SELECTOR" ]]; then
	KUBE_ARGS+=( -l "$LABEL_SELECTOR" )
fi
if [[ -n "$FIELD_SELECTOR" ]]; then
	KUBE_ARGS+=( --field-selector "$FIELD_SELECTOR" )
fi
if [[ -n "$CHUNK" ]]; then
	KUBE_ARGS+=( --chunk-size "$CHUNK" )
fi

echo "Scanning for token (masked): $(mask "$TOKEN")"

# Prefetch ConfigMaps and Secrets into temp files (base64-encoded values)
CM_TMP=$(mktemp)
SECRET_TMP=$(mktemp)
trap 'rm -f "$CM_TMP" "$SECRET_TMP"' EXIT

echo "Prefetching ConfigMaps and Secrets (reduces per-ref kubectl calls)..."
# ConfigMaps: store namespace, name, key, base64(value)
kubectl get configmaps "${KUBE_ARGS[@]}" -o json | \
	jq -r '.items[] | .metadata as $m | (.data // {}) | to_entries[] | [$m.namespace,$m.name,.key,(.value|@base64)] | @tsv' > "$CM_TMP"

# Secrets: store namespace, name, key, base64(value) (value already base64 in secret.data)
kubectl get secrets "${KUBE_ARGS[@]}" -o json | \
	jq -r '.items[] | .metadata as $m | (.data // {}) | to_entries[] | [$m.namespace,$m.name,.key,.value] | @tsv' > "$SECRET_TMP"

echo "Scanning ConfigMaps from cache..."
# Scan cached ConfigMaps for token
while IFS=$'\t' read -r ns name key b64val; do
	if echo "$b64val" | base64 --decode 2>/dev/null | grep -a -F -- "$TOKEN" >/dev/null 2>&1; then
		report "ConfigMap $ns/$name key=$key contains token"
	fi
done < "$CM_TMP"

echo "Scanning Secrets from cache..."
# Scan cached Secrets for token
while IFS=$'\t' read -r ns name key b64val; do
	if [[ -z "$b64val" ]]; then continue; fi
	if echo "$b64val" | base64 --decode 2>/dev/null | grep -a -F -- "$TOKEN" >/dev/null 2>&1; then
		report "Secret $ns/$name key=$key contains token (decoded)"
	fi
	# Also check base64 representation match
	if [[ "$b64val" == "$TOKEN_B64" ]]; then
		report "Secret $ns/$name key=$key stores the token (base64 match)"
	fi
done < "$SECRET_TMP"

echo "Scanning Pods' environment variables and references..."
# 1) Direct env values in containers and initContainers
kubectl get pods "${KUBE_ARGS[@]}" -o json | \
	jq -r --arg token "$TOKEN" '.items[] | .metadata as $m | (.spec.containers + (.spec.initContainers // []))[]? as $c | (.env // [])[]? | select(.value != null and (.value | index($token))) | "pod\t" + $m.namespace + "\t" + $m.name + "\t" + $c.name + "\t" + .name' | \
	while IFS=$'\t' read -r kind ns pod container var; do
		report "Pod $ns/$pod container=$container env var=$var contains token"
	done

# Note: To strictly follow the requested checks we only inspect:
# 1) direct pod environment variable values
# 2) ConfigMap contents
# 3) Secret contents
# The script intentionally does not resolve or follow env.valueFrom/secretKeyRef/configMapKeyRef or envFrom
# references here — all ConfigMaps and Secrets have already been prefetched and scanned above.

echo "Scan complete."
if [[ "$FOUND" -eq 1 ]]; then
	echo "One or more findings reported above." >&2
	exit 1
else
	echo "No occurrences of the token found in pods, configmaps, or secrets."
	exit 0
fi

